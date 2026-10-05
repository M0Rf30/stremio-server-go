// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"archive/zip"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func writeTestZip(t *testing.T, entry string, content []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create(entry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestArchiveExtractEntry_SingleFlight: N concurrent requests for the same
// uncached entry must trigger exactly one extraction and share its path.
func TestArchiveExtractEntry_SingleFlight(t *testing.T) {
	content := []byte("single flight payload")
	sess := &archiveSession{
		archivePath: writeTestZip(t, "movie.mkv", content),
		ext:         "zip",
		tmpDir:      t.TempDir(),
		extracted:   map[string]string{},
	}

	var extractions atomic.Int32
	release := make(chan struct{})
	archiveExtractTestHook = func(string) {
		extractions.Add(1)
		<-release
	}
	t.Cleanup(func() { archiveExtractTestHook = nil })

	const n = 8
	var started atomic.Int32
	var wg sync.WaitGroup
	paths := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started.Add(1)
			paths[i], errs[i] = archiveExtractEntry(sess, "movie.mkv")
		}()
	}
	// Give every goroutine the chance to reach archiveExtractEntry while the
	// first extraction is still blocked, then let it finish.
	for started.Load() < n {
		runtime.Gosched()
	}
	for range 200 {
		runtime.Gosched()
	}
	close(release)
	wg.Wait()

	if got := extractions.Load(); got != 1 {
		t.Fatalf("extractions = %d, want 1", got)
	}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		if paths[i] != paths[0] {
			t.Errorf("call %d path %q != %q", i, paths[i], paths[0])
		}
	}
	got, err := os.ReadFile(paths[0])
	if err != nil || string(got) != string(content) {
		t.Fatalf("extracted content = %q, %v", got, err)
	}
	if ents, _ := os.ReadDir(sess.tmpDir); len(ents) != 1 {
		t.Errorf("tmpDir has %d files, want 1", len(ents))
	}
	sess.mu.Lock()
	left := len(sess.inflight)
	sess.mu.Unlock()
	if left != 0 {
		t.Errorf("inflight not cleaned: %d", left)
	}
}

// TestArchiveExtractEntry_FailureNotCached: a failed extraction is shared by
// no later call; the next request retries.
func TestArchiveExtractEntry_FailureNotCached(t *testing.T) {
	sess := &archiveSession{
		archivePath: writeTestZip(t, "a.bin", []byte("x")),
		ext:         "zip",
		tmpDir:      t.TempDir(),
	}
	if _, err := archiveExtractEntry(sess, "missing.bin"); err == nil {
		t.Fatal("expected error for missing entry")
	}
	if p, err := archiveExtractEntry(sess, "a.bin"); err != nil || p == "" {
		t.Fatalf("retry after failure: %q, %v", p, err)
	}
}

func chtimesAgo(t *testing.T, p string, d time.Duration) {
	t.Helper()
	old := time.Now().Add(-d)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestArchiveSweepStale(t *testing.T) {
	root := t.TempDir()
	mk := func(name string, dir bool, age time.Duration) string {
		p := filepath.Join(root, name)
		var err error
		if dir {
			err = os.MkdirAll(filepath.Join(p, "entry-1"), 0o700)
		} else {
			err = os.WriteFile(p, []byte("x"), 0o600)
		}
		if err != nil {
			t.Fatal(err)
		}
		chtimesAgo(t, p, age)
		return p
	}
	staleDir := mk("stremio-archive-aaa", true, 3*time.Hour)
	staleDL := mk("stremio-archive-dl-bbb", false, 3*time.Hour)
	freshDir := mk("stremio-archive-ccc", true, time.Minute)
	liveDir := mk("stremio-archive-live", true, 3*time.Hour)
	liveArch := mk("stremio-archive-dl-live", false, 3*time.Hour)
	foreignDir := mk("other-stale", true, 3*time.Hour)
	foreignFile := mk("stremio-archive-mine.zip", false, 3*time.Hour) // not a dl file
	nzbDir := mk("stremio-nzb-zzz", true, 3*time.Hour)                // other janitor's prefix

	archiveSessionsMu.Lock()
	archiveSessions["sweep-live"] = &archiveSession{key: "sweep-live", tmpDir: liveDir, archivePath: liveArch}
	archiveSessionsMu.Unlock()
	t.Cleanup(func() {
		archiveSessionsMu.Lock()
		delete(archiveSessions, "sweep-live")
		archiveSessionsMu.Unlock()
	})

	archiveSweepStale(root)

	for _, p := range []string{staleDir, staleDL} {
		if exists(p) {
			t.Errorf("%s should have been swept", filepath.Base(p))
		}
	}
	for _, p := range []string{freshDir, liveDir, liveArch, foreignDir, foreignFile, nzbDir} {
		if !exists(p) {
			t.Errorf("%s must be kept", filepath.Base(p))
		}
	}
}

func TestNzbSweepStale(t *testing.T) {
	root := t.TempDir()
	mk := func(name string, age time.Duration) string {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		chtimesAgo(t, p, age)
		return p
	}
	stale := mk("stremio-nzb-aaa", 3*time.Hour)
	fresh := mk("stremio-nzb-bbb", time.Minute)
	live := mk("stremio-nzb-live", 3*time.Hour)
	foreign := mk("stremio-archive-old", 3*time.Hour)

	nzbSessionsMu.Lock()
	nzbSessions["sweep-live"] = &nzbSession{key: "sweep-live", tmpDir: live}
	nzbSessionsMu.Unlock()
	t.Cleanup(func() {
		nzbSessionsMu.Lock()
		delete(nzbSessions, "sweep-live")
		nzbSessionsMu.Unlock()
	})

	nzbSweepStale(root)

	if exists(stale) {
		t.Error("stale nzb dir should have been swept")
	}
	for _, p := range []string{fresh, live, foreign} {
		if !exists(p) {
			t.Errorf("%s must be kept", filepath.Base(p))
		}
	}
}
