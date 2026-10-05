// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package media

// Regression tests for the P2 media review findings: self-origin loopback
// fetches, relay streaming/token bounds, idle-eviction vs StartHLS, and
// strict Range handling in OpenSubHash. All offline (httptest, t.TempDir,
// PATH-shim ffmpeg).

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/types"
)

// ── finding: self-origin carve-out must work with the real transports ───────

func serveContentHandler(content []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "video.bin", time.Time{}, bytes.NewReader(content))
	})
}

func TestOpenSubHashSelfOriginLoopbackRealTransport(t *testing.T) {
	const fileSize = 150_000
	content := make([]byte, fileSize)
	for i := range content {
		content[i] = byte(i * 7)
	}
	ts := httptest.NewServer(serveContentHandler(content))
	defer ts.Close()

	// No transport stubbing: ts is on 127.0.0.1, which openSubClient blocks.
	p := &prober{baseURLLocal: ts.URL}
	res, err := p.OpenSubHash(ts.URL + "/video.bin")
	if err != nil {
		t.Fatalf("OpenSubHash against own loopback origin: %v", err)
	}
	m := res.(map[string]interface{})
	want := fmt.Sprintf("%016x", computeOpenSubHash(fileSize, content[:chunkSize], content[fileSize-chunkSize:]))
	if m["hash"] != want {
		t.Errorf("hash = %v, want %s", m["hash"], want)
	}

	// A loopback URL that is NOT the configured self origin stays blocked.
	other := &prober{baseURLLocal: "http://127.0.0.1:1"}
	if _, err := other.OpenSubHash(ts.URL + "/video.bin"); err == nil {
		t.Error("OpenSubHash fetched a non-self loopback URL; SSRF guard bypassed")
	}
}

func TestFetchSubBytesSelfOriginLoopbackRealTransport(t *testing.T) {
	const srt = "1\n00:00:01,000 --> 00:00:02,000\nhi\n"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, srt)
	}))
	defer ts.Close()

	got, err := fetchSubBytes(ts.URL+"/s.srt", ts.URL)
	if err != nil {
		t.Fatalf("fetchSubBytes self origin: %v", err)
	}
	if string(got) != srt {
		t.Errorf("body = %q, want %q", got, srt)
	}
	if _, err := fetchSubBytes(ts.URL+"/s.srt", ""); err == nil {
		t.Error("fetchSubBytes without selfBase fetched a loopback URL")
	}
}

func TestSelfOriginClientRefusesOffOriginRedirect(t *testing.T) {
	secret := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "internal secret")
	}))
	defer secret.Close()
	self := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, secret.URL+"/x", http.StatusFound)
	}))
	defer self.Close()

	if _, err := fetchSubBytes(self.URL+"/s.srt", self.URL); err == nil {
		t.Fatal("self-origin client followed a redirect to another loopback origin")
	}
}

// ── finding: httpRangeGet strictness ────────────────────────────────────────

func TestHTTPRangeGetRejectsIgnoredRangeAndShortBodies(t *testing.T) {
	full := bytes.Repeat([]byte{0xAB}, 3*chunkSize)
	client := &http.Client{}

	t.Run("200 for non-zero offset is rejected", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(full) // ignores Range
		}))
		defer ts.Close()
		if _, err := httpRangeGet(client, ts.URL, int64(2*chunkSize), int64(3*chunkSize-1)); err == nil {
			t.Error("200 response accepted for a tail request; would hash the wrong bytes")
		}
		// From offset 0 a 200 body is a valid prefix.
		got, err := httpRangeGet(client, ts.URL, 0, chunkSize-1)
		if err != nil || len(got) != chunkSize {
			t.Errorf("200 prefix: len=%d err=%v, want %d bytes", len(got), err, chunkSize)
		}
	})

	t.Run("short 206 body is rejected", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", chunkSize-1, len(full)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(full[:100])
		}))
		defer ts.Close()
		if _, err := httpRangeGet(client, ts.URL, 0, chunkSize-1); err == nil {
			t.Error("short body accepted")
		}
	})

	t.Run("mismatched Content-Range is rejected", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", chunkSize-1, len(full)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(full[:chunkSize])
		}))
		defer ts.Close()
		if _, err := httpRangeGet(client, ts.URL, int64(chunkSize), int64(2*chunkSize-1)); err == nil {
			t.Error("206 with a different Content-Range accepted")
		}
	})

	t.Run("exact 206 accepted", func(t *testing.T) {
		ts := httptest.NewServer(serveContentHandler(full))
		defer ts.Close()
		got, err := httpRangeGet(client, ts.URL, int64(chunkSize), int64(2*chunkSize-1))
		if err != nil || len(got) != chunkSize {
			t.Errorf("len=%d err=%v, want %d bytes", len(got), err, chunkSize)
		}
	})
}

func TestOpenSubHashRejectsServerIgnoringRange(t *testing.T) {
	content := bytes.Repeat([]byte{1}, 200_000)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Advertises the length on HEAD but never honours Range.
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(content)
	}))
	defer ts.Close()
	p := &prober{baseURLLocal: ts.URL}
	if _, err := p.OpenSubHash(ts.URL + "/v"); err == nil {
		t.Error("OpenSubHash returned a hash although the server ignores Range")
	}
}

func TestOpenSubHashSmallFile(t *testing.T) {
	content := bytes.Repeat([]byte{3}, 1000) // smaller than one 64 KiB window
	ts := httptest.NewServer(serveContentHandler(content))
	defer ts.Close()
	p := &prober{baseURLLocal: ts.URL}
	res, err := p.OpenSubHash(ts.URL + "/v")
	if err != nil {
		t.Fatalf("OpenSubHash small file: %v", err)
	}
	want := fmt.Sprintf("%016x", computeOpenSubHash(1000, content, content))
	if got := res.(map[string]interface{})["hash"]; got != want {
		t.Errorf("hash = %v, want %s", got, want)
	}
}

// ── finding: relay must not impose a body deadline; truncation must surface ──

func TestMediaRelayClientHasNoBodyTimeout(t *testing.T) {
	r := newMediaRelay(false)
	if r.client.Timeout != 0 {
		t.Errorf("relay client Timeout = %v; it would truncate streamed bodies", r.client.Timeout)
	}
	tr, ok := r.client.Transport.(*http.Transport)
	if !ok || tr.ResponseHeaderTimeout <= 0 {
		t.Error("relay transport must bound time-to-first-byte via ResponseHeaderTimeout")
	}
}

func TestMediaRelayAbortsOnTruncatedUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		// Chunked response that dies mid-stream: no Content-Length.
		_, _ = io.WriteString(w, strings.Repeat("x", 1024))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	defer upstream.Close()

	r := newMediaRelay(false)
	relayURL, err := r.register(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(relayURL) //nolint:gosec // our own loopback listener
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Error("truncated upstream looked like a clean end-of-body to the relay client")
	}
}

// ── finding: relay token map is bounded and deduplicated ────────────────────

func TestMediaRelayRegisterDedupesPerURL(t *testing.T) {
	r := newMediaRelay(false)
	a, err := r.register("http://93.184.216.34/a.ts")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := r.register("http://93.184.216.34/a.ts")
	if a != b {
		t.Errorf("same upstream URL got two tokens: %q vs %q", a, b)
	}
	c, _ := r.register("http://93.184.216.34/c.ts")
	if a == c {
		t.Error("different upstream URLs shared a token")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.tokens) != 2 {
		t.Errorf("tokens = %d, want 2", len(r.tokens))
	}
}

func TestMediaRelayTokenCap(t *testing.T) {
	r := newMediaRelay(false)
	var first, last string
	for i := range relayMaxTokens + 500 {
		u, err := r.register(fmt.Sprintf("http://93.184.216.34/seg%d.ts", i))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = u
		}
		last = u
	}
	r.mu.Lock()
	n, nURL := len(r.tokens), len(r.byURL)
	r.mu.Unlock()
	if n > relayMaxTokens || nURL > relayMaxTokens {
		t.Errorf("tokens=%d byURL=%d, want <= %d", n, nURL, relayMaxTokens)
	}
	tok := func(u string) string { return u[strings.LastIndex(u, "/")+1:] }
	if _, ok := r.resolve(tok(first)); ok {
		t.Error("oldest token survived past the cap")
	}
	if got, ok := r.resolve(tok(last)); !ok || !strings.HasSuffix(got, fmt.Sprintf("seg%d.ts", relayMaxTokens+499)) {
		t.Errorf("newest token not resolvable: %q ok=%v", got, ok)
	}
}

func TestMediaRelayTokenCapSweepsExpiredFirst(t *testing.T) {
	r := newMediaRelay(false)
	for i := range relayMaxTokens {
		if _, err := r.register(fmt.Sprintf("http://93.184.216.34/old%d.ts", i)); err != nil {
			t.Fatal(err)
		}
	}
	// Expire the 100 most recent registrations; the oldest must survive.
	r.mu.Lock()
	for _, tok := range r.order[len(r.order)-100:] {
		e := r.tokens[tok]
		e.expiresAt = time.Now().Add(-time.Minute)
		r.tokens[tok] = e
	}
	r.mu.Unlock()

	u, err := r.register("http://93.184.216.34/fresh.ts")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.resolve(u[strings.LastIndex(u, "/")+1:]); !ok {
		t.Error("fresh token missing")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.tokens) != relayMaxTokens-100+1 {
		t.Errorf("tokens = %d, want %d (expired swept, no live evictions)", len(r.tokens), relayMaxTokens-100+1)
	}
}

// ── finding: evictIdle parks the id so a same-id StartHLS can't be wiped ────

func TestEvictIdleParksIdInDrainingUntilDirGone(t *testing.T) {
	m := newTestHLSManager(t)
	s := injectSession(t, m, "resume")
	s.lastAccess.Store(time.Now().Add(-10 * m.cfg.SessionTTL).UnixNano())

	m.evictIdle()
	if dirExists(s.dir) {
		t.Error("evicted session dir still exists")
	}
	m.mu.Lock()
	nSess, nDrain := len(m.sessions), len(m.draining)
	m.mu.Unlock()
	if nSess != 0 || nDrain != 0 {
		t.Errorf("after eviction sessions=%d draining=%d, want 0/0", nSess, nDrain)
	}

	// Simulate the window between unregistering and RemoveAll finishing (the
	// removal is claimed but not complete): the id must be refused, not
	// recreated and then wiped.
	s2 := injectSession(t, m, "resume")
	s2.lastAccess.Store(time.Now().Add(-10 * m.cfg.SessionTTL).UnixNano())
	s2.removed.Store(true) // removeDeletedSession becomes a no-op: draining entry stays
	m.evictIdle()

	m.mu.Lock()
	_, parked := m.draining["resume"]
	m.mu.Unlock()
	if !parked {
		t.Fatal("evicted session id not parked in draining")
	}
	_, err := m.StartHLS("resume", "http://93.184.216.34/x.mkv", types.HLSSessionOptions{})
	if err == nil || !strings.Contains(err.Error(), "still being deleted") {
		t.Errorf("StartHLS during eviction window = %v, want 'still being deleted' refusal", err)
	}
}

// ── finding: truncated subtitle extraction must not be cached ───────────────

func TestExtractSubtitleDoesNotCacheTruncatedOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH-shim ffmpeg stub is a /bin/sh script")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "ffmpeg")
	// Writes a partial track to the output (last argv) and reports a premature
	// end on stderr while still exiting 0, like ffmpeg does.
	script := "#!/bin/sh\nfor a; do out=$a; done\nprintf 'WEBVTT\\n' > \"$out\"\n" +
		"echo 'Stream ends prematurely at 5, should be 10' >&2\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	m := newTestHLSManager(t)
	m.selfBase = "http://127.0.0.1:11470"
	s := injectSession(t, m, "sub")
	s.mediaURL = m.selfBase + "/x/0"

	if _, err := m.extractSubtitle(context.Background(), s, 0); err == nil {
		t.Fatal("extractSubtitle accepted a prematurely-ended input")
	}
	if dirExists(filepath.Join(s.dir, "sub0.vtt")) {
		t.Error("truncated subtitle was promoted to the permanent cache")
	}
	if dirExists(filepath.Join(s.dir, "sub0.vtt.tmp")) {
		t.Error("tmp file left behind")
	}

	// A clean run (no premature-end message) is cached.
	script = "#!/bin/sh\nfor a; do out=$a; done\nprintf 'WEBVTT\\n' > \"$out\"\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := m.extractSubtitle(context.Background(), s, 0)
	if err != nil {
		t.Fatalf("clean extraction failed: %v", err)
	}
	if !dirExists(p) {
		t.Error("clean extraction not cached")
	}
}
