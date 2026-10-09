// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Windows refuses to delete (or replace) a file that still has an open handle.
// Linux does not, so these tests observe the handle table instead of waiting for
// a Windows failure: they count the file descriptors this process holds on a
// path (/proc/self/fd) and record the open handles at the moment the server
// removes a temp file. Without /proc (Windows, macOS) they skip.
//
// What they pin down:
//   - an archive session keeps no handle on the archive file between requests —
//     not even while a background extraction or CRC verification is paused —
//     so the user can delete or replace their own archive and a downloaded temp
//     archive can be removed;
//   - a temp entry file is removed only after its last handle (the extraction
//     writer and every progress reader) is closed;
//   - tearing a session down waits for its background goroutines to let go of
//     the files before it removes them.

const archFDTable = "/proc/self/fd"

func archRequireFDTable(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(archFDTable); err != nil {
		t.Skip("needs /proc/self/fd to observe open file handles")
	}
}

// archOpenHandles lists the targets of this process' open file descriptors that
// are path itself or, for a directory, anything beneath it.
func archOpenHandles(path string) []string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	ents, err := os.ReadDir(archFDTable)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		target, err := os.Readlink(filepath.Join(archFDTable, e.Name()))
		if err != nil {
			continue // closed meanwhile
		}
		target = strings.TrimSuffix(target, " (deleted)")
		if target == path || strings.HasPrefix(target, path+string(filepath.Separator)) {
			out = append(out, target)
		}
	}
	return out
}

// archExpectNoHandles reports a handle on path (or below it) that is still open
// after a grace period. The grace covers the instant between a response being
// complete for the client and its handler returning (closing its file).
func archExpectNoHandles(t *testing.T, when, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		hs := archOpenHandles(path)
		if len(hs) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("%s: %d handle(s) still open on %s: %v", when, len(hs), path, hs)
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// archRemovalWatch records every removal of a temp file or directory under
// scope, and what was wrong with it at that moment.
type archRemovalWatch struct {
	mu      sync.Mutex
	removed []string
	bad     []string
}

// archWatchRemovals watches removals under scope. extra, when set, is called
// for each one and returns a non-empty complaint when something is wrong.
func archWatchRemovals(t *testing.T, scope string, extra func(path string) string) *archRemovalWatch {
	t.Helper()
	w := &archRemovalWatch{}
	hook := func(path string) {
		if path != scope && !strings.HasPrefix(path, scope+string(filepath.Separator)) {
			return
		}
		var complaint string
		if hs := archOpenHandles(path); len(hs) > 0 {
			complaint = fmt.Sprintf("%s removed while %d handle(s) were still open: %v", path, len(hs), hs)
		} else if extra != nil {
			complaint = extra(path)
		}
		w.mu.Lock()
		w.removed = append(w.removed, path)
		if complaint != "" {
			w.bad = append(w.bad, complaint)
		}
		w.mu.Unlock()
	}
	archiveRemoveTestHook.Store(&hook)
	t.Cleanup(func() { archiveRemoveTestHook.Store(nil) })
	return w
}

func (w *archRemovalWatch) snapshot() (removed, bad []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.removed...), append([]string(nil), w.bad...)
}

func (w *archRemovalWatch) expectClean(t *testing.T) {
	t.Helper()
	_, bad := w.snapshot()
	for _, b := range bad {
		t.Error(b)
	}
}

func archFlight(sess *archiveSession, name string) *archiveExtractFlight {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.inflight[name]
}

// archTgz writes a gzip-compressed tar holding one entry and returns its path.
func archTgz(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "prog.tgz")
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// archRemoveEventually removes path, retrying briefly: on Windows a handle that
// a finishing request or goroutine is about to close makes the first attempt
// fail. The server keeps no handle on a local archive, so it must succeed.
func archRemoveEventually(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := os.Remove(path)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("could not remove %s: %v", path, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ── no handle on the archive between requests ────────────────────────────────

// The extraction and the CRC verification run in the background after the
// response is out. Parked mid-way — between two reads — they must not hold the
// user's archive open: only the read in progress needs it.
func TestArchiveHandles_LocalArchiveNotHeldWhileBackgroundWorkIsParked(t *testing.T) {
	archRequireFDTable(t)

	t.Run("deflated zip extraction", func(t *testing.T) {
		p := progZip(t, progEntry{"movie.mkv", zip.Deflate, progNoise(2<<20, 71)})
		h := newHandler(t)
		key, sess := progCreate(t, h, p, "zip")
		gate, _ := progInstallHooks(t, sess, 256<<10)
		if rec := progGet(h, http.MethodGet, "zip", key, "movie.mkv", "bytes=0-99"); rec.Code != http.StatusPartialContent {
			t.Fatalf("status %d", rec.Code)
		}
		fl := archFlight(sess, "movie.mkv")
		if fl == nil {
			t.Fatal("the extraction finished before it could be parked")
		}
		auditEventually(t, 15*time.Second, "extraction to park", func() bool { return fl.progress() >= 256<<10 })
		if hs := archOpenHandles(p); len(hs) != 0 {
			t.Errorf("response is out and the extraction is parked, yet the archive is held open: %v", hs)
		}
		gate.Release()
		progWaitFlights(t, sess)
		archExpectNoHandles(t, "after the extraction", p)
	})

	t.Run("tgz extraction", func(t *testing.T) {
		p := archTgz(t, "movie.mkv", progNoise(2<<20, 72))
		h := newHandler(t)
		key, sess := progCreate(t, h, p, "tgz")
		gate, _ := progInstallHooks(t, sess, 256<<10)
		if rec := progGet(h, http.MethodGet, "tgz", key, "movie.mkv", "bytes=0-99"); rec.Code != http.StatusPartialContent {
			t.Fatalf("status %d", rec.Code)
		}
		fl := archFlight(sess, "movie.mkv")
		if fl == nil {
			t.Fatal("the extraction finished before it could be parked")
		}
		auditEventually(t, 15*time.Second, "extraction to park", func() bool { return fl.progress() >= 256<<10 })
		if hs := archOpenHandles(p); len(hs) != 0 {
			t.Errorf("response is out and the extraction is parked, yet the archive is held open: %v", hs)
		}
		gate.Release()
		progWaitFlights(t, sess)
		archExpectNoHandles(t, "after the extraction", p)
	})

	t.Run("stored zip verification", func(t *testing.T) {
		setEagerVerify(t, 4<<10) // serve at once; the verification runs on
		p := progZip(t, progEntry{"movie.mkv", zip.Store, progNoise(1<<20, 73)})
		h := newHandler(t)
		key, sess := progCreate(t, h, p, "zip")

		release := make(chan struct{})
		var once sync.Once
		releaseGate := func() { once.Do(func() { close(release) }) }
		var parked atomic.Bool
		archiveVerifyTestHook = func(_ string, off int64) {
			if off >= 256<<10 {
				parked.Store(true)
				<-release
			}
		}
		t.Cleanup(func() { archiveVerifyTestHook = nil })
		t.Cleanup(func() {
			releaseGate()
			if v := directVerifyIfAny(sess, "movie.mkv"); v != nil {
				<-v.done
			}
		})

		if rec := progGet(h, http.MethodGet, "zip", key, "movie.mkv", "bytes=0-9"); rec.Code != http.StatusPartialContent {
			t.Fatalf("status %d", rec.Code)
		}
		auditEventually(t, 15*time.Second, "verification to park", parked.Load)
		if hs := archOpenHandles(p); len(hs) != 0 {
			t.Errorf("response is out and the verification is parked, yet the archive is held open: %v", hs)
		}
		releaseGate()
		v := directVerify(t, sess, "movie.mkv")
		<-v.done
		archExpectNoHandles(t, "after the verification", p)
	})
}

func directVerifyIfAny(sess *archiveSession, name string) *archiveVerify {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.direct[name].v
}

// Whatever the format and however the entry is served (in place, extracted,
// whole, ranged), nothing — no archive, no temp entry — stays open once the
// requests and their background work are over.
func TestArchiveHandles_NoneHeldBetweenRequests(t *testing.T) {
	archRequireFDTable(t)
	content := progNoise(600<<10, 74)
	cases := []struct {
		name, ext string
		mk        func(t *testing.T) string
	}{
		{"zip deflated", "zip", func(t *testing.T) string { return progZip(t, progEntry{"movie.mkv", zip.Deflate, content}) }},
		{"zip stored", "zip", func(t *testing.T) string { return progZip(t, progEntry{"movie.mkv", zip.Store, content}) }},
		{"tar", "tar", func(t *testing.T) string { return progTar(t, progEntry{"movie.mkv", 0, content}) }},
		{"tgz", "tgz", func(t *testing.T) string { return archTgz(t, "movie.mkv", content) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.mk(t)
			h := newHandler(t)
			key, sess := progCreate(t, h, p, tc.ext)
			archExpectNoHandles(t, "after create", p)

			for _, rng := range []string{"", "bytes=100-4999", "bytes=-64", "bytes=300000-"} {
				rec := progGet(h, http.MethodGet, tc.ext, key, "movie.mkv", rng)
				if rec.Code != http.StatusOK && rec.Code != http.StatusPartialContent {
					t.Fatalf("Range %q: status %d", rng, rec.Code)
				}
				archExpectNoHandles(t, fmt.Sprintf("after GET %q", rng), p)
			}
			progWaitFlights(t, sess)
			archExpectNoHandles(t, "after the extractions", p)
			archExpectNoHandles(t, "after the extractions", sess.tmpDir)
		})
	}
}

// ── a temp entry file is removed only after its last handle is closed ───────

// archCorruptedZip writes a zip holding one deflated entry and lets mutate
// damage its raw bytes (dataOff is where the entry's compressed data starts).
func archCorruptedZip(t *testing.T, name string, content []byte, mutate func(raw []byte, dataOff int64)) string {
	t.Helper()
	p := progZip(t, progEntry{name, zip.Deflate, content})
	zr, err := zip.OpenReader(p)
	if err != nil {
		t.Fatal(err)
	}
	off, err := zr.File[0].DataOffset()
	_ = zr.Close()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	mutate(raw, off)
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A failed extraction deletes its temp file. The writer's handle is closed by
// then, but the request that was waiting for the first byte opened the file too:
// on Windows the removal fails (and the file is left behind) unless that reader
// has let go first.
func TestArchiveHandles_FailedExtractionRemovesFileAfterReaderClosed(t *testing.T) {
	archRequireFDTable(t)

	t.Run("fails before the first byte", func(t *testing.T) {
		// BTYPE=3 is reserved: the very first read fails.
		p := archCorruptedZip(t, "bad.mkv", bytes.Repeat([]byte("hello "), 5000), func(raw []byte, off int64) { raw[off] = 0x07 })
		h := newHandler(t)
		key, sess := progCreate(t, h, p, "zip")
		w := archWatchRemovals(t, sess.tmpDir, nil)

		// Hold the extraction at its start until the request has opened the temp
		// file: two handles (writer + reader) are then open on it.
		release := make(chan struct{})
		var once sync.Once
		releaseGate := func() { once.Do(func() { close(release) }) }
		archiveExtractTestHook = func(string) { <-release }
		t.Cleanup(func() { archiveExtractTestHook = nil })
		t.Cleanup(releaseGate)

		got := make(chan *httptest.ResponseRecorder, 1)
		go func() { got <- progGet(h, http.MethodGet, "zip", key, "bad.mkv", "") }()
		auditEventually(t, 15*time.Second, "the request to open the temp file next to the writer", func() bool {
			return len(archOpenHandles(sess.tmpDir)) >= 2
		})
		releaseGate()

		select {
		case rec := <-got:
			if rec.Code != http.StatusInternalServerError || !strings.HasPrefix(rec.Body.String(), "extract: ") {
				t.Errorf("status %d body %q, want 500 \"extract: …\"", rec.Code, rec.Body.String())
			}
		case <-time.After(15 * time.Second):
			t.Fatal("request did not finish")
		}
		progWaitFlights(t, sess)
		auditEventually(t, 5*time.Second, "the failed temp file to be removed", func() bool {
			ents, _ := os.ReadDir(sess.tmpDir)
			return len(ents) == 0
		})
		removed, _ := w.snapshot()
		if len(removed) == 0 {
			t.Error("the failed extraction's temp file was never removed")
		}
		w.expectClean(t)
	})

	t.Run("fails at the end, after bytes were served", func(t *testing.T) {
		content := progNoise(1<<20, 75)
		// Flip a payload byte: the CRC-32 mismatch only shows at EOF, while the
		// request is still reading the growing file.
		p := archCorruptedZip(t, "movie.mkv", content, func(raw []byte, off int64) { raw[off+int64(len(content))/2] ^= 0xff })
		h := newHandler(t)
		key, sess := progCreate(t, h, p, "zip")
		w := archWatchRemovals(t, sess.tmpDir, nil)

		rec := progGet(h, http.MethodGet, "zip", key, "movie.mkv", "")
		if rec.Code == http.StatusOK && rec.Body.Len() >= len(content) {
			t.Fatalf("corrupt entry was delivered complete (%d bytes)", rec.Body.Len())
		}
		progWaitFlights(t, sess)
		auditEventually(t, 5*time.Second, "the failed temp file to be removed", func() bool {
			ents, _ := os.ReadDir(sess.tmpDir)
			return len(ents) == 0
		})
		removed, _ := w.snapshot()
		if len(removed) == 0 {
			t.Error("the failed extraction's temp file was never removed")
		}
		w.expectClean(t)
	})
}

// ── tearing a session down ───────────────────────────────────────────────────

// Destroying a session cancels its background extraction and then removes its
// temp dir and its downloaded archive. The extraction goroutine may still be
// inside a read of that archive (or, for rar/7z, hold it for the whole entry):
// removing first leaves the downloaded archive — possibly gigabytes — behind on
// Windows. The teardown must wait for the goroutine to be done.
func TestArchiveDestroySession_WaitsForBackgroundWorkBeforeRemoving(t *testing.T) {
	archRequireFDTable(t)
	archReuseEnv(t) // downloads land in the test's own temp dir
	data, err := os.ReadFile(progZip(t, progEntry{"movie.mkv", zip.Deflate, progNoise(2<<20, 76)}))
	if err != nil {
		t.Fatal(err)
	}
	origin := newArchReuseOrigin(t, data)
	srv := httptest.NewServer(newHandler(t))
	t.Cleanup(srv.Close)
	key := archReuseKey(t)
	payload := fmt.Sprintf(`{"url":%q}`, origin.srv.URL+"/a.zip")
	loc, st := archReuseCreate(t, srv.URL, "zip", key, payload)
	if st != http.StatusTemporaryRedirect {
		t.Fatalf("create: status %d", st)
	}
	sess := archReuseSession(key)
	if sess == nil || !sess.isTempArch {
		t.Fatal("expected a session owning a downloaded archive")
	}
	gate, _ := progInstallHooks(t, sess, 256<<10)

	if code, _ := archReuseGet(t, srv.URL, loc, "bytes=0-99"); code != http.StatusPartialContent {
		t.Fatalf("range GET: status %d", code)
	}
	fl := archFlight(sess, "movie.mkv")
	if fl == nil {
		t.Fatal("the extraction finished before it could be parked")
	}
	auditEventually(t, 15*time.Second, "extraction to park", func() bool { return fl.progress() >= 256<<10 })

	scope := filepath.Dir(sess.archivePath)
	w := archWatchRemovals(t, scope, func(path string) string {
		if path == fl.path {
			return "" // the extraction discarding its own partial file
		}
		select {
		case <-fl.done:
			return ""
		default:
			return path + " removed while its extraction goroutine was still running"
		}
	})

	archiveSessionsMu.Lock()
	delete(archiveSessions, key)
	archiveSessionsMu.Unlock()
	destroyed := make(chan struct{})
	go func() {
		archiveDestroySession(sess)
		close(destroyed)
	}()
	select {
	case <-destroyed:
		t.Error("teardown returned (and removed the files) while the extraction goroutine was still running")
	case <-time.After(300 * time.Millisecond):
	}
	gate.Release()
	select {
	case <-destroyed:
	case <-time.After(15 * time.Second):
		t.Fatal("teardown did not finish once the extraction stopped")
	}
	if exists(sess.archivePath) || exists(sess.tmpDir) {
		t.Errorf("teardown left files behind: archive %v, tmp dir %v", exists(sess.archivePath), exists(sess.tmpDir))
	}
	if removed, _ := w.snapshot(); len(removed) < 2 {
		t.Errorf("removals seen: %v, want the temp dir and the downloaded archive", removed)
	}
	w.expectClean(t)
}
