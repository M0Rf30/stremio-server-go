// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/archive"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// setEagerVerify lowers the size up to which a stored entry is verified before
// its first byte is served.
func setEagerVerify(t *testing.T, n int64) {
	t.Helper()
	old := archiveEagerVerifyBytes
	archiveEagerVerifyBytes = n
	t.Cleanup(func() { archiveEagerVerifyBytes = old })
}

// verifyGate parks every CRC verification once it is about to hash a byte at or
// past limit, until Release; starts counts verifications that began.
type verifyGate struct {
	ch     chan struct{}
	once   sync.Once
	limit  int64
	starts atomic.Int32
}

func (g *verifyGate) Release() { g.once.Do(func() { close(g.ch) }) }

func installVerifyGate(t *testing.T, limit int64) *verifyGate {
	t.Helper()
	g := &verifyGate{ch: make(chan struct{}), limit: limit}
	prev := archiveVerifyTestHook
	archiveVerifyTestHook = func(_ string, off int64) {
		if off == 0 {
			g.starts.Add(1)
		}
		if off >= g.limit {
			<-g.ch
		}
	}
	t.Cleanup(func() {
		g.Release()
		archiveVerifyTestHook = prev
	})
	return g
}

// directVerify returns the verification of a session's in-place entry.
func directVerify(t *testing.T, sess *archiveSession, name string) *archiveVerify {
	t.Helper()
	sess.mu.Lock()
	defer sess.mu.Unlock()
	d, ok := sess.direct[name]
	if !ok || d.v == nil {
		t.Fatalf("no verification recorded for %q (ok=%v)", name, ok)
	}
	return d.v
}

type bgResponse struct {
	code int
	body []byte
	err  error
}

// bgGet runs a GET against url in the background (real HTTP, so partial bodies
// and aborted responses are observable) and delivers its outcome.
func bgGet(url, rng string) <-chan bgResponse {
	out := make(chan bgResponse, 1)
	go func() {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			out <- bgResponse{err: err}
			return
		}
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			out <- bgResponse{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		out <- bgResponse{code: resp.StatusCode, body: body, err: err}
	}()
	return out
}

func mustNotFinishWithin(t *testing.T, ch <-chan bgResponse, d time.Duration, why string) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("%s (status %d, %d bytes, err %v)", why, r.code, len(r.body), r.err)
	case <-time.After(d):
	}
}

func mustFinish(t *testing.T, ch <-chan bgResponse) bgResponse {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(15 * time.Second):
		t.Fatal("request did not finish")
		return bgResponse{}
	}
}

func serveHandler(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// ── in-place CRC verification ────────────────────────────────────────────────

// A large corrupt stored entry starts serving at once (in place), but its final
// chunk is withheld until the background verification ran — and then the
// response aborts instead of looking complete.
func TestArchiveStream_LargeCorruptStoredEntryNeverLooksComplete(t *testing.T) {
	setEagerVerify(t, 4<<10)
	content := progNoise(1<<20, 31)
	p := progZip(t, progEntry{"movie.mkv", zip.Store, content})
	corruptStoredPayload(t, p, 0, 700_000)
	onDisk := bytes.Clone(content)
	onDisk[700_000] ^= 0xff // what the in-place reader actually hands out
	h := newHandler(t)
	key, _ := progCreate(t, h, p, "zip")
	gate := installVerifyGate(t, 256<<10)
	url := serveHandler(t, h).URL + "/zip/stream/" + key + "/movie.mkv"

	// Unverified head ranges are answered while verification is still parked.
	head := mustFinish(t, bgGet(url, "bytes=0-1023"))
	if head.err != nil || head.code != http.StatusPartialContent || !bytes.Equal(head.body, content[:1024]) {
		t.Fatalf("head range while verifying: status %d err %v", head.code, head.err)
	}

	// The whole entry streams (all but the tail) without waiting for the CRC.
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	first := make([]byte, len(content)-128<<10)
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatalf("progressive part of an in-place response: %v", err)
	}
	if !bytes.Equal(first, onDisk[:len(first)]) {
		t.Fatal("progressive part differs from the entry's bytes")
	}
	rest := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, resp.Body); rest <- err }()
	select {
	case err := <-rest:
		t.Fatalf("the entry's end was delivered before verification finished (err %v)", err)
	case <-time.After(100 * time.Millisecond):
	}

	gate.Release()
	select {
	case err := <-rest:
		if err == nil {
			t.Fatal("corrupt stored entry was delivered as a clean complete response")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("response never ended after verification finished")
	}

	// From now on the verdict is known: a clean 500, no bytes.
	if rec := progGet(h, http.MethodGet, "zip", key, "movie.mkv", "bytes=0-9"); rec.Code != http.StatusInternalServerError {
		t.Errorf("request after a failed verification: status %d, want 500", rec.Code)
	}
}

// A healthy large stored entry: no extraction, no temp bytes, one verification
// shared by every request; only reads reaching the entry's end wait for it.
func TestArchiveStream_LargeStoredEntryVerifiedOnceAndServedInPlace(t *testing.T) {
	setEagerVerify(t, 4<<10)
	content := progNoise(1<<20+777, 32)
	h := newHandler(t)
	key, sess := progCreate(t, h, progZip(t, progEntry{"movie.mkv", zip.Store, content}), "zip")
	var extractions atomic.Int32
	archiveExtractTestHook = func(string) { extractions.Add(1) }
	t.Cleanup(func() { archiveExtractTestHook = nil })
	gate := installVerifyGate(t, 256<<10)
	url := serveHandler(t, h).URL + "/zip/stream/" + key + "/movie.mkv"

	// Concurrent first requests share one verification.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := <-bgGet(url, "bytes=0-9"); r.err != nil || r.code != http.StatusPartialContent || !bytes.Equal(r.body, content[:10]) {
				t.Errorf("head range: status %d err %v", r.code, r.err)
			}
		}()
	}
	wg.Wait()

	tail := bgGet(url, "bytes=-100")
	whole := bgGet(url, "")
	mustNotFinishWithin(t, tail, 100*time.Millisecond, "tail range answered before verification finished")
	mustNotFinishWithin(t, whole, 50*time.Millisecond, "whole entry completed before verification finished")

	gate.Release()
	if r := mustFinish(t, tail); r.err != nil || r.code != http.StatusPartialContent || !bytes.Equal(r.body, content[len(content)-100:]) {
		t.Errorf("tail range after verification: status %d err %v", r.code, r.err)
	}
	if r := mustFinish(t, whole); r.err != nil || r.code != http.StatusOK || !bytes.Equal(r.body, content) {
		t.Errorf("whole entry after verification: status %d err %v, %d bytes", r.code, r.err, len(r.body))
	}

	if err := directVerify(t, sess, "movie.mkv").wait(context.Background()); err != nil {
		t.Errorf("verification verdict = %v, want nil", err)
	}
	// Verified: later tail reads no longer wait on anything.
	if r := mustFinish(t, bgGet(url, "bytes=-10")); r.err != nil || !bytes.Equal(r.body, content[len(content)-10:]) {
		t.Errorf("tail after verification: err %v", r.err)
	}
	if n := gate.starts.Load(); n != 1 {
		t.Errorf("verifications started = %d, want exactly 1 per session and entry", n)
	}
	if n := extractions.Load(); n != 0 {
		t.Errorf("stored entry was extracted %d time(s); it must be served in place", n)
	}
	if ents, _ := os.ReadDir(sess.tmpDir); len(ents) != 0 {
		t.Errorf("tmpDir holds %d file(s); in-place serving must not write temp bytes", len(ents))
	}
}

// Small stored entries are verified before the first byte, so even a ranged
// response cannot carry unverified bytes; the extra latency is the hash only.
func TestArchiveStream_SmallStoredEntryVerifiedBeforeFirstByte(t *testing.T) {
	content := progNoise(100<<10, 33)
	h := newHandler(t)
	key, _ := progCreate(t, h, progZip(t, progEntry{"movie.mkv", zip.Store, content}), "zip")
	gate := installVerifyGate(t, 0) // park before the very first read
	url := serveHandler(t, h).URL + "/zip/stream/" + key + "/movie.mkv"

	ranged := bgGet(url, "bytes=5-20")
	mustNotFinishWithin(t, ranged, 100*time.Millisecond, "ranged response started before the entry was verified")
	gate.Release()
	if r := mustFinish(t, ranged); r.err != nil || r.code != http.StatusPartialContent || !bytes.Equal(r.body, content[5:21]) {
		t.Errorf("ranged response: status %d err %v", r.code, r.err)
	}
}

// Tar records no content checksum: nothing to verify, served at once.
func TestArchiveStream_TarEntriesAreNotVerified(t *testing.T) {
	content := progNoise(300<<10, 34)
	h := newHandler(t)
	key, sess := progCreate(t, h, progTar(t, progEntry{"movie.mkv", 0, content}), "tar")
	gate := installVerifyGate(t, 0)
	if rec := progGet(h, http.MethodGet, "tar", key, "movie.mkv", ""); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), content) {
		t.Fatalf("tar entry: status %d", rec.Code)
	}
	if n := gate.starts.Load(); n != 0 {
		t.Errorf("a tar entry started %d verification(s)", n)
	}
	sess.mu.Lock()
	d := sess.direct["movie.mkv"]
	sess.mu.Unlock()
	if !d.ok || d.v != nil {
		t.Errorf("tar direct = ok %v v %v; want served in place without a verification", d.ok, d.v)
	}
}

// Destroying a session stops its verifications.
func TestArchiveDestroySession_CancelsVerification(t *testing.T) {
	setEagerVerify(t, 4<<10)
	h := newHandler(t)
	key, sess := progCreate(t, h, progZip(t, progEntry{"movie.mkv", zip.Store, progNoise(1<<20, 35)}), "zip")
	gate := installVerifyGate(t, 256<<10)
	if rec := progGet(h, http.MethodGet, "zip", key, "movie.mkv", "bytes=0-9"); rec.Code != http.StatusPartialContent {
		t.Fatalf("status %d", rec.Code)
	}
	v := directVerify(t, sess, "movie.mkv")
	auditEventually(t, 15*time.Second, "verification to reach the gate", func() bool { return gate.starts.Load() == 1 })

	archiveSessionsMu.Lock()
	delete(archiveSessions, key)
	archiveSessionsMu.Unlock()
	archiveDestroySession(sess)
	gate.Release()
	select {
	case <-v.done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled verification did not stop")
	}
	if !errors.Is(v.err, context.Canceled) {
		t.Errorf("verification err = %v, want context.Canceled", v.err)
	}
}

// A verification that fails for a reason other than a mismatch (here: it was
// cancelled) is not a verdict on the data: the entry is extracted — and thereby
// verified by zip itself — instead of being served unverified.
func TestArchiveStream_UnverifiableInPlaceEntryFallsBackToExtraction(t *testing.T) {
	content := progNoise(300<<10, 36)
	h := newHandler(t)
	key, sess := progCreate(t, h, progZip(t, progEntry{"movie.mkv", zip.Store, content}), "zip")
	gate := installVerifyGate(t, 0)

	d := archiveDirectExtent(sess, "movie.mkv")
	if !d.ok || d.v == nil {
		t.Fatalf("direct = %+v, want an in-place entry under verification", d)
	}
	auditEventually(t, 15*time.Second, "verification to start", func() bool { return gate.starts.Load() == 1 })
	d.v.cancel()
	gate.Release()
	<-d.v.done
	if !errors.Is(d.v.err, context.Canceled) {
		t.Fatalf("verification err = %v, want context.Canceled", d.v.err)
	}

	rec := progGet(h, http.MethodGet, "zip", key, "movie.mkv", "")
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), content) {
		t.Fatalf("fallback response: status %d, %d bytes", rec.Code, rec.Body.Len())
	}
	progWaitFlights(t, sess)
	sess.mu.Lock()
	nExtracted, direct := len(sess.extracted), sess.direct["movie.mkv"]
	sess.mu.Unlock()
	if nExtracted != 1 || direct.ok {
		t.Errorf("extracted=%d direct.ok=%v; want the entry extracted and no longer served in place", nExtracted, direct.ok)
	}
}

// ── retired sessions ─────────────────────────────────────────────────────────

func newRetiredFixture(t *testing.T, key string, pins int) *archiveSession {
	t.Helper()
	s := capSession(t, key, 0, 0)
	for range pins {
		if archiveAcquireSession(key) != s {
			t.Fatal("acquire returned the wrong session")
		}
	}
	t.Cleanup(func() {
		archiveSessionsMu.Lock()
		delete(archiveRetired, s)
		archiveSessionsMu.Unlock()
	})
	return s
}

// Retiring a pinned session keeps it alive; only the last release destroys it.
func TestArchiveRetire_LastReleaseDestroys(t *testing.T) {
	isolateArchiveSessions(t)
	s := newRetiredFixture(t, "retire", 2)

	archiveSessionsMu.Lock()
	delete(archiveSessions, "retire")
	destroyNow := archiveRetireLocked(s)
	archiveSessionsMu.Unlock()
	if destroyNow {
		t.Fatal("a pinned session must not be destroyed on replacement")
	}
	if archiveAcquireSession("retire") != nil {
		t.Fatal("a retired session must not be acquirable any more")
	}

	s.release()
	time.Sleep(50 * time.Millisecond)
	if !exists(s.tmpDir) {
		t.Fatal("session destroyed while a request still reads it")
	}
	s.release()
	auditEventually(t, 5*time.Second, "retired session destroyed by its last release", func() bool { return !exists(s.tmpDir) })
	archiveSessionsMu.Lock()
	_, still := archiveRetired[s]
	archiveSessionsMu.Unlock()
	if still {
		t.Error("destroyed session is still registered as retired")
	}
}

func TestArchiveRetire_UnpinnedSessionIsDestroyedNow(t *testing.T) {
	isolateArchiveSessions(t)
	s := capSession(t, "idle", 0, 0)
	archiveSessionsMu.Lock()
	delete(archiveSessions, "idle")
	destroyNow := archiveRetireLocked(s)
	_, registered := archiveRetired[s]
	archiveSessionsMu.Unlock()
	if !destroyNow || registered {
		t.Errorf("destroyNow=%v registered=%v, want true/false", destroyNow, registered)
	}
}

// A retired session's temp files are not reaped as stale, and its bytes count
// against the global byte cap.
func TestArchiveRetire_SweepAndCapsSeeRetiredSessions(t *testing.T) {
	isolateArchiveSessions(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	old := time.Now().Add(-2 * archiveSessionTTL)
	mkStale := func(name string) string {
		d := filepath.Join(tmp, archiveTmpDirPrefix+name)
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
		return d
	}
	retiredDir, orphanDir := mkStale("retired"), mkStale("orphan")

	setArchiveCaps(32, 100)
	keep := capSession(t, "keep", 0, 0)
	idle := capSession(t, "idle", time.Minute, 20)
	r := newRetiredFixture(t, "r", 1)
	r.tmpDir, r.archiveBytes = retiredDir, 90
	archiveSessionsMu.Lock()
	delete(archiveSessions, "r")
	archiveRetireLocked(r)
	archiveSessionsMu.Unlock()

	archiveSweepStale(tmp)
	if !exists(retiredDir) {
		t.Error("sweeper reaped the temp dir of a retired session that is still being read")
	}
	if exists(orphanDir) {
		t.Error("sweeper kept a genuinely stale dir")
	}

	archiveEnforceCaps(keep) // 90 (retired) + 20 = 110 > 100: only `idle` can go
	if hasSession("idle") {
		t.Error("retired session's bytes were not counted against the byte cap")
	}
	_ = idle
}

// Re-creating one key over and over while streams are running must never cut a
// stream short, and must not leak the replaced sessions.
func TestArchiveCreate_RecreateStormKeepsEveryStreamComplete(t *testing.T) {
	content := progNoise(768<<10, 41)
	p := progZip(t, progEntry{"movie.mkv", zip.Deflate, content})
	h := newHandler(t)
	key, _ := progCreate(t, h, p, "zip")
	t.Cleanup(func() {
		archiveSessionsMu.Lock()
		cur := archiveSessions[key]
		delete(archiveSessions, key)
		archiveSessionsMu.Unlock()
		if cur != nil {
			archiveDestroySession(cur)
		}
	})
	body, err := json.Marshal(map[string]string{"url": p})
	if err != nil {
		t.Fatal(err)
	}

	var (
		wg   sync.WaitGroup
		bad  atomic.Int32
		done atomic.Bool
		reqs atomic.Int32
	)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !done.Load() {
				rec := progGet(h, http.MethodGet, "zip", key, "movie.mkv", "")
				reqs.Add(1)
				if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), content) {
					bad.Add(1)
				}
			}
		}()
	}
	for i := range 25 {
		req := httptest.NewRequest(http.MethodPost, "/zip/create/"+key, bytes.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("re-create #%d: status %d", i, rec.Code)
		}
		time.Sleep(3 * time.Millisecond)
	}
	done.Store(true)
	wg.Wait()

	if n := bad.Load(); n != 0 {
		t.Errorf("%d of %d streams were cut short or failed while the key was being re-created", n, reqs.Load())
	}
	auditEventually(t, 10*time.Second, "all replaced sessions to be destroyed", func() bool {
		archiveSessionsMu.Lock()
		defer archiveSessionsMu.Unlock()
		return len(archiveRetired) == 0
	})
}

// Verify extents agree with the archive package's notion of the entry.
func TestArchiveDirectExtent_CarriesRecordedCRC(t *testing.T) {
	content := progNoise(10<<10, 37)
	h := newHandler(t)
	_, sess := progCreate(t, h, progZip(t, progEntry{"a.bin", zip.Store, content}), "zip")
	d := archiveDirectExtent(sess, "a.bin")
	if !d.ok || !d.ext.HasCRC || d.v == nil {
		t.Fatalf("direct = %+v", d)
	}
	if err := d.v.wait(context.Background()); err != nil {
		t.Errorf("verdict = %v", err)
	}
	if !errors.Is(archive.ErrChecksum, zip.ErrChecksum) {
		t.Error("archive.ErrChecksum must be zip.ErrChecksum")
	}
}
