// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

// Package api — regression tests for NZB session replacement and teardown:
//
//   - GET /nzb/create/{key}?lz= is a stream URL (307) that players re-request on
//     every open/seek, so re-creating a key must never cancel the assembly a
//     still-running response is reading from; the replaced session is retired
//     when its last request finishes.
//   - a session's temp file must be closed before its directory is deleted
//     (Windows refuses to delete a file that is still open).
package api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	lzstring "github.com/daku10/go-lz-string"
)

// nzbReplaceEnv lets nzbCreate fetch from loopback and keeps the temp dirs it
// creates inside the test's own directory tree.
func nzbReplaceEnv(t *testing.T) {
	t.Helper()
	t.Setenv("STREMIO_ARCHIVE_ALLOW_PRIVATE", "1")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
}

// recreate re-creates the fixture's session key through the player-facing
// GET /nzb/create/{key}?lz= form and returns the redirect Location.
func (fx *nzbStreamFixture) recreate(t *testing.T) string {
	t.Helper()
	nzbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-nzb")
		_, _ = w.Write([]byte(minimalNZBXML))
	}))
	t.Cleanup(nzbSrv.Close)

	payload := fmt.Sprintf(`{"nzbUrl":"%s/replacement.nzb","servers":["nntp://127.0.0.1:9"]}`, nzbSrv.URL)
	lz, err := lzstring.CompressToEncodedURIComponent(payload)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	req, err := http.NewRequestWithContext(tctx(t, 15*time.Second), http.MethodGet,
		fx.srv.URL+"/nzb/create/"+url.PathEscape(fx.sess.key)+"?lz="+url.QueryEscape(lz), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("GET /nzb/create/{key}?lz= = %d, want 307", resp.StatusCode)
	}
	// The replacement session now owns the key; retire it with the test.
	t.Cleanup(func() {
		nzbSessionsMu.Lock()
		cur := nzbSessions[fx.sess.key]
		if cur != nil && cur != fx.sess {
			delete(nzbSessions, fx.sess.key)
		}
		nzbSessionsMu.Unlock()
		if cur != nil && cur != fx.sess {
			cur.discard()
		}
	})
	return resp.Header.Get("Location")
}

// streamResult is the outcome of a full-file GET running in the background.
type streamResult struct {
	status int
	body   []byte
	err    error
}

// startFullGet starts an unranged GET of the fixture's file in the background.
// started is closed once the first body bytes have arrived (the response is
// then in flight, reading from the live assembly).
func (fx *nzbStreamFixture) startFullGet(t *testing.T) (started <-chan struct{}, result <-chan streamResult) {
	t.Helper()
	ctx := tctx(t, 60*time.Second)
	first := make(chan struct{})
	res := make(chan streamResult, 1)
	go func() {
		var r streamResult
		defer func() { res <- r }()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fx.url, nil)
		if err != nil {
			r.err = err
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			r.err = err
			return
		}
		defer resp.Body.Close()
		r.status = resp.StatusCode
		var once sync.Once
		var buf bytes.Buffer
		chunk := make([]byte, 32<<10)
		for {
			n, err := resp.Body.Read(chunk)
			if n > 0 {
				buf.Write(chunk[:n])
				once.Do(func() { close(first) })
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				r.err = err
				break
			}
		}
		r.body = buf.Bytes()
	}()
	return first, res
}

// waitInFlight blocks until a response is mid-stream on the fixture's session.
func (fx *nzbStreamFixture) waitInFlight(t *testing.T, started <-chan struct{}, wantBytes int64) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("response never started")
	}
	asm := fx.assembly()
	if asm == nil {
		t.Fatal("no assembly after the response started")
	}
	if err := asm.WaitRange(tctx(t, 15*time.Second), 0, wantBytes); err != nil {
		t.Fatalf("waiting for the first %d bytes: %v", wantBytes, err)
	}
	nzbSessionsMu.Lock()
	refs := fx.sess.refCount
	nzbSessionsMu.Unlock()
	if refs < 1 {
		t.Fatalf("refCount = %d; want an in-flight request", refs)
	}
}

func waitGone(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for exists(path) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if exists(path) {
		t.Errorf("%s still present", path)
	}
}

// Replacing a session key while a full GET is still streaming from the old
// session must not truncate that response (ExoPlayer re-requests the create
// URL on every open/seek). The old session is retired afterwards.
func TestNzbCreate_ReplaceKeyWhileStreamingKeepsResponseIntact(t *testing.T) {
	nzbReplaceEnv(t)
	fx := newNzbStreamFixture(t, "movie.mkv", 10, 8_000, 0, 2)
	release := fx.gateFrom(t, 3) // segments 3.. are withheld: the GET stalls mid-file

	started, result := fx.startFullGet(t)
	fx.waitInFlight(t, started, 3*8_000)
	asm := fx.assembly()

	if loc := fx.recreate(t); loc == "" {
		t.Fatal("create returned no Location")
	}
	nzbSessionsMu.Lock()
	cur := nzbSessions[fx.sess.key]
	nzbSessionsMu.Unlock()
	if cur == nil || cur == fx.sess {
		t.Fatalf("key still maps to the old session (%p)", cur)
	}

	// Give a (buggy) asynchronous discard every chance to run.
	select {
	case <-asm.Done():
		t.Error("replacing the key cancelled the assembly behind an in-flight response")
	case <-time.After(300 * time.Millisecond):
	}
	release()

	var res streamResult
	select {
	case res = <-result:
	case <-time.After(30 * time.Second):
		t.Fatal("response never finished")
	}
	if res.err != nil {
		t.Fatalf("response aborted after %d of %d bytes: %v", len(res.body), len(fx.content), res.err)
	}
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.status)
	}
	if !bytes.Equal(res.body, fx.content) {
		t.Fatalf("got %d bytes, want %d identical bytes", len(res.body), len(fx.content))
	}

	// The last release retires the replaced session: workers stopped,
	// connections released, temp dir gone.
	select {
	case <-asm.Done():
	case <-time.After(10 * time.Second):
		t.Error("replaced session's assembly never finished")
	}
	waitGone(t, fx.sess.tmpDir)
	nzbSessionsMu.Lock()
	_, orphan := nzbOrphans[fx.sess]
	nzbSessionsMu.Unlock()
	if orphan {
		t.Error("retired session still tracked as an orphan")
	}
}

// With no request in flight the replaced session is still discarded at once
// (assembly cancelled, temp dir removed) — replacement behaves as before.
func TestNzbCreate_ReplaceIdleKeyDiscardsOldSession(t *testing.T) {
	nzbReplaceEnv(t)
	fx := newNzbStreamFixture(t, "movie.mkv", 6, 5_000, 0, 2)
	fx.gateFrom(t, 2)
	resp, err := fx.get(tctx(t, 15*time.Second), t, "bytes=0-9")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	asm := fx.assembly()
	if asm == nil || asm.Complete() {
		t.Fatal("expected a running assembly")
	}
	// The handler's deferred release runs just after the body is delivered.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		nzbSessionsMu.Lock()
		refs := fx.sess.refCount
		nzbSessionsMu.Unlock()
		if refs == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	fx.recreate(t)

	select {
	case <-asm.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("replacing an idle session left its assembly running")
	}
	if asm.Err() == nil {
		t.Error("discarded assembly must report cancellation")
	}
	waitGone(t, fx.sess.tmpDir)
}

// A replaced session that is still streaming is no longer in nzbSessions, but
// its temp dir must not be swept as a leak while a response reads from it.
func TestNzbSweepStale_KeepsOrphanedSessionDir(t *testing.T) {
	nzbReplaceEnv(t)
	root := t.TempDir()
	dir := filepath.Join(root, nzbTmpDirPrefix+"orphan")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fx := newNzbStreamFixture(t, "movie.mkv", 10, 8_000, 0, 2)
	nzbSessionsMu.Lock()
	fx.sess.tmpDir = dir // before the first request, under the registry lock
	nzbSessionsMu.Unlock()
	release := fx.gateFrom(t, 3)

	started, result := fx.startFullGet(t)
	fx.waitInFlight(t, started, 3*8_000)
	fx.recreate(t)

	chtimesAgo(t, dir, 3*time.Hour)
	nzbSweepStale(root)
	if !exists(dir) {
		t.Fatal("temp dir of a session that is still streaming was deleted")
	}

	release()
	select {
	case res := <-result:
		if res.err != nil || !bytes.Equal(res.body, fx.content) {
			t.Fatalf("response damaged: %d/%d bytes, err=%v", len(res.body), len(fx.content), res.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("response never finished")
	}
	waitGone(t, dir)
}

// discard must not delete the temp dir while an assembly's temp file is still
// open: Windows refuses to delete an open file, which would leak the dir (and
// up to the file's size) until the 1h stale sweep. The close is made to block
// so the ordering is asserted deterministically instead of racing.
func TestNzbSession_DiscardClosesTempFileBeforeRemovingDir(t *testing.T) {
	nzbReplaceEnv(t)
	fx := newNzbStreamFixture(t, "movie.mkv", 6, 5_000, 0, 2)
	fx.gateFrom(t, 2)

	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock) // registered after the fixture's: runs before its discard
	closed := make(chan struct{})
	nzbSessionsMu.Lock()
	fx.sess.closeFile = func(f *os.File) error {
		enterOnce.Do(func() { close(entered) })
		<-release
		err := f.Close()
		close(closed)
		return err
	}
	nzbSessionsMu.Unlock()

	resp, err := fx.get(tctx(t, 15*time.Second), t, "bytes=0-9")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	discarded := make(chan struct{})
	go func() { fx.sess.discard(); close(discarded) }()

	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("temp file close never started")
	}
	// The file is still open: discard must hold back the delete.
	select {
	case <-discarded:
		t.Fatal("discard returned while the temp file was still open")
	case <-time.After(300 * time.Millisecond):
	}
	if !exists(fx.sess.tmpDir) {
		t.Fatal("temp dir deleted while its file was still open")
	}

	unblock()
	select {
	case <-discarded:
	case <-time.After(15 * time.Second):
		t.Fatal("discard hung after the file was closed")
	}
	select {
	case <-closed:
	default:
		t.Error("discard returned before the temp file was closed")
	}
	if exists(fx.sess.tmpDir) {
		t.Error("temp dir still present after discard")
	}
}
