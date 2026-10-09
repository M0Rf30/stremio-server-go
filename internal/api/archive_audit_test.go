// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"archive/zip"
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// Regression tests for the adversarial review of the progressive/in-place
// archive serving work: session replacement vs. live streams, the byte cap vs.
// zero-byte sessions, and CRC verification of stored zip entries served in place.

// auditEventually polls cond until it holds or the deadline passes.
func auditEventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// ── re-creating a key must not kill streams reading the old session ──────────

// ExoPlayer re-requests /{ext}/create/{key}?lz= (a 307 stream URL) on every
// open/seek. Re-creating a session under the same key must replace it for new
// requests, but a stream already reading the old session must run to the end.
func TestArchiveCreate_ReplacingKeyDoesNotKillLiveStream(t *testing.T) {
	content := progNoise(3<<20, 21)
	p := progZip(t, progEntry{"movie.mkv", zip.Deflate, content})
	h := newHandler(t)
	key, old := progCreate(t, h, p, "zip")
	gate, _ := progInstallHooks(t, old, 256<<10)

	got := make(chan *httptest.ResponseRecorder, 1)
	go func() { got <- progGet(h, http.MethodGet, "zip", key, "movie.mkv", "") }()

	// The request is pinned to the old session and its extraction is parked at
	// the gate with the first chunk(s) readable.
	auditEventually(t, 15*time.Second, "stream pinned to the old session with a gated extraction", func() bool {
		old.mu.Lock()
		defer old.mu.Unlock()
		fl := old.inflight["movie.mkv"]
		return old.refCount > 0 && fl != nil && fl.progress() >= 256<<10
	})

	// The same key is created again with another payload (new Range/seek round
	// trip; an identical payload would reuse the session, see archive_reuse_test.go).
	_, fresh := progCreateWith(t, h, p, "zip", map[string]any{"fileMustInclude": "movie"})
	if fresh == old {
		t.Fatal("create did not allocate a new session")
	}

	// Give any (buggy) asynchronous teardown of the replaced session time to run
	// before the extraction is allowed to continue.
	settle := time.Now().Add(150 * time.Millisecond)
	for exists(old.tmpDir) && time.Now().Before(settle) {
		time.Sleep(time.Millisecond)
	}
	gate.Release()

	var rec *httptest.ResponseRecorder
	select {
	case rec = <-got:
	case <-time.After(15 * time.Second):
		t.Fatal("live stream never finished")
	}
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), content) {
		t.Fatalf("live stream was interrupted by the session being re-created: status %d, got %d of %d bytes",
			rec.Code, rec.Body.Len(), len(content))
	}

	// The replaced session is not leaked: it goes away once its stream is done.
	auditEventually(t, 5*time.Second, "replaced session cleanup after its last stream", func() bool {
		return !exists(old.tmpDir)
	})

	// And the new session serves.
	if rec := progGet(h, http.MethodGet, "zip", key, "movie.mkv", "bytes=0-99"); rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), content[:100]) {
		t.Errorf("new session: status %d", rec.Code)
	}
}

// With no request in flight the replaced session is torn down promptly, as before.
func TestArchiveCreate_ReplacingIdleKeyDestroysOldSession(t *testing.T) {
	p := progZip(t, progEntry{"movie.mkv", zip.Deflate, progNoise(64<<10, 22)})
	h := newHandler(t)
	key, old := progCreate(t, h, p, "zip")
	if rec := progGet(h, http.MethodGet, "zip", key, "movie.mkv", ""); rec.Code != http.StatusOK {
		t.Fatalf("warm-up stream: status %d", rec.Code)
	}
	_, fresh := progCreateWith(t, h, p, "zip", map[string]any{"fileMustInclude": "movie"})
	if fresh == old {
		t.Fatal("create did not allocate a new session")
	}
	auditEventually(t, 5*time.Second, "idle replaced session cleanup", func() bool { return !exists(old.tmpDir) })
}

// ── byte cap vs. sessions that hold no bytes ─────────────────────────────────

// When only the byte cap is exceeded and the overage is held by the session
// being served, wiping idle sessions that hold no bytes frees nothing and just
// 404s other users' next Range request.
func TestArchiveEnforceCaps_ByteCapSparesZeroByteSessions(t *testing.T) {
	isolateArchiveSessions(t)
	setArchiveCaps(32, 100)
	keep := capSession(t, "keep", 0, 200) // in-place/local sessions below hold nothing
	capSession(t, "z1", 3*time.Minute, 0)
	capSession(t, "z2", 2*time.Minute, 0)
	capSession(t, "z3", time.Minute, 0)

	archiveEnforceCaps(keep)

	for _, k := range []string{"z1", "z2", "z3", "keep"} {
		if !hasSession(k) {
			t.Errorf("session %q was evicted although the byte overage is not reclaimable from it", k)
		}
	}
}

// Zero-byte sessions are skipped, byte-holding ones still go LRU-first until
// the total fits.
func TestArchiveEnforceCaps_ByteCapEvictsOnlyByteHolders(t *testing.T) {
	isolateArchiveSessions(t)
	setArchiveCaps(32, 100)
	keep := capSession(t, "keep", 0, 50)
	capSession(t, "zero", 5*time.Minute, 0) // oldest, but frees nothing
	capSession(t, "a", 4*time.Minute, 60)
	capSession(t, "b", 3*time.Minute, 60)

	archiveEnforceCaps(keep) // 170 > 100: a (→110) then b (→50)

	if hasSession("a") || hasSession("b") {
		t.Error("byte-holding idle sessions should be evicted to fit the byte cap")
	}
	if !hasSession("zero") || !hasSession("keep") {
		t.Error("zero-byte session (and the kept one) must survive a byte-cap-only eviction")
	}
}

// If even evicting every idle session cannot bring the total under the cap,
// nothing is evicted for the byte cap: it would hurt other users for no gain.
func TestArchiveEnforceCaps_ByteCapStopsWhenUnreachable(t *testing.T) {
	isolateArchiveSessions(t)
	setArchiveCaps(32, 100)
	keep := capSession(t, "keep", 0, 200)
	capSession(t, "a", 2*time.Minute, 30)
	capSession(t, "b", time.Minute, 30)

	archiveEnforceCaps(keep)

	if !hasSession("a") || !hasSession("b") {
		t.Error("idle sessions evicted although the byte cap stays exceeded by the kept session")
	}
}

// The session-count cap still evicts LRU-first regardless of bytes.
func TestArchiveEnforceCaps_CountCapStillEvictsZeroByteSessions(t *testing.T) {
	isolateArchiveSessions(t)
	setArchiveCaps(2, 100)
	keep := capSession(t, "keep", 0, 0)
	capSession(t, "old", 3*time.Minute, 0)
	capSession(t, "mid", 2*time.Minute, 0)

	archiveEnforceCaps(keep)

	if hasSession("old") || !hasSession("mid") || !hasSession("keep") {
		t.Errorf("count cap: old=%v mid=%v keep=%v, want false/true/true", hasSession("old"), hasSession("mid"), hasSession("keep"))
	}
}

// ── stored zip entries served in place must still be CRC-checked ─────────────

// corruptStoredPayload flips one payload byte of a stored zip entry in place,
// leaving the recorded CRC-32 untouched.
func corruptStoredPayload(t *testing.T, path string, entry int, at int64) {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	off, err := zr.File[entry].DataOffset()
	_ = zr.Close()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[off+at] ^= 0xff
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Before the in-place change a corrupt stored entry failed its CRC check at the
// end of f.Open's read (→ 500). Serving it in place must not turn that into a
// clean, complete 200 response.
func TestArchiveStream_CorruptStoredEntryNeverServedAsCleanComplete(t *testing.T) {
	content := progNoise(1<<20, 23)
	p := progZip(t, progEntry{"movie.mkv", zip.Store, content})
	corruptStoredPayload(t, p, 0, int64(len(content))/2)

	h := newHandler(t)
	key, _ := progCreate(t, h, p, "zip")

	rec := progGet(h, http.MethodGet, "zip", key, "movie.mkv", "")
	if rec.Code == http.StatusOK && rec.Body.Len() >= len(content) {
		t.Fatalf("corrupt stored entry was served as a clean complete 200 (%d bytes)", rec.Body.Len())
	}
	// Small entries are verified before the first byte, so even a ranged
	// request cannot hand out unverified bytes.
	rec = progGet(h, http.MethodGet, "zip", key, "movie.mkv", "bytes=10-99")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("ranged request on a corrupt small stored entry: status %d, want 500", rec.Code)
	}
}
