// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package ftpstream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// resetFTPCaches empties the idle pool and the size/pre-flight caches now and
// again at test end, so tests never observe each other's sessions (an ephemeral
// loopback port can be reused by the next test's fake server).
func resetFTPCaches(tb testing.TB) {
	tb.Helper()
	reset := func() {
		ftpPoolMu.Lock()
		var all []*ftpSession
		for _, idle := range ftpPool {
			all = append(all, idle...)
		}
		ftpPool = map[ftpPoolKey][]*ftpSession{}
		ftpPoolIdle = 0
		ftpPoolMu.Unlock()
		for _, s := range all {
			s.discard()
		}
		ftpMetaMu.Lock()
		clear(ftpSizes)
		clear(preflights)
		ftpMetaMu.Unlock()
	}
	reset()
	tb.Cleanup(reset)
}

// poolIdle reports the number of idle pooled sessions.
func poolIdle() int {
	ftpPoolMu.Lock()
	defer ftpPoolMu.Unlock()
	return ftpPoolIdle
}

// poolTestContent is a deterministic payload large enough to seek around in.
func poolTestContent(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}

// readRange opens rawURL at off, reads up to n bytes and closes the stream,
// mimicking one short ranged /ftp request. It returns the bytes read and the
// size reported by Open.
func readRange(tb testing.TB, rawURL string, off int64, n int) ([]byte, int64) {
	tb.Helper()
	rc, size, err := openFTP(tb.Context(), rawURL, off)
	if err != nil {
		tb.Fatalf("openFTP(off=%d): %v", off, err)
	}
	buf := make([]byte, n)
	got, err := io.ReadFull(rc, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		tb.Fatalf("read(off=%d): %v", off, err)
	}
	// A 426 from an early close is a normal server answer; the control
	// connection stays usable, so the error is not interesting here.
	_ = rc.Close()
	return buf[:got], size
}

// seekOffsets is a typical player pattern: head probe, tail probe (moov),
// then forward/backward seeks.
var seekOffsets = []int64{0, 9000, 100, 7000, 8000, 50, 5000, 9500, 1, 4000}

// TestOpenFTPSequentialRangesReuseSession measures the cost of N sequential
// short ranged opens against one server. Before connection pooling every open
// dialled a fresh control connection and logged in again (10 opens → 10
// control connections, 10 logins, 10 SIZE); with the pool N opens cost exactly
// one control connection, one login and (SIZE cached for seeks) one SIZE.
func TestOpenFTPSequentialRangesReuseSession(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	resetFTPCaches(t)
	content := poolTestContent(10000)
	srv := newFakeFTPServer(t, &fakeFTPServer{content: content})
	u := "ftp://" + srv.addr() + "/movie.mkv"

	for _, off := range seekOffsets {
		got, size := readRange(t, u, off, 64)
		if size != int64(len(content)) {
			t.Fatalf("off=%d size = %d, want %d", off, size, len(content))
		}
		want := content[off:min(int(off)+64, len(content))]
		if !bytes.Equal(got, want) {
			t.Fatalf("off=%d got %q, want %q", off, got, want)
		}
	}
	n := int64(len(seekOffsets))
	t.Logf("opens=%d control_conns=%d logins=%d size_cmds=%d retr=%d rest=%d",
		n, srv.conns.Load(), srv.logins.Load(), srv.sizes.Load(), srv.retrs.Load(), srv.rests.Load())
	if srv.conns.Load() != 1 || srv.logins.Load() != 1 {
		t.Errorf("control conns = %d, logins = %d; want 1 and 1 for %d sequential opens",
			srv.conns.Load(), srv.logins.Load(), n)
	}
	// Only the offset-0 open asks the server; every seek uses the cached size.
	if got := srv.sizes.Load(); got != 1 {
		t.Errorf("SIZE commands = %d, want 1", got)
	}
	if srv.retrs.Load() != n {
		t.Errorf("RETR = %d, want %d", srv.retrs.Load(), n)
	}
	if got := poolIdle(); got != 1 {
		t.Errorf("idle sessions = %d, want 1", got)
	}
}

// TestOpenFTPOffsetZeroRefreshesSize verifies a playback start (offset 0)
// never trusts the cached SIZE, while seeks do.
func TestOpenFTPOffsetZeroRefreshesSize(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	resetFTPCaches(t)
	srv := newFakeFTPServer(t, &fakeFTPServer{content: poolTestContent(1000)})
	u := "ftp://" + srv.addr() + "/f"

	readRange(t, u, 0, 8)
	readRange(t, u, 0, 8)
	if got := srv.sizes.Load(); got != 2 {
		t.Errorf("SIZE after two offset-0 opens = %d, want 2", got)
	}
	readRange(t, u, 500, 8)
	readRange(t, u, 600, 8)
	if got := srv.sizes.Load(); got != 2 {
		t.Errorf("SIZE after two seeks = %d, want still 2 (cached)", got)
	}
}

// TestOpenFTPSizeCacheExpires verifies an expired cached SIZE is re-fetched.
func TestOpenFTPSizeCacheExpires(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	resetFTPCaches(t)
	srv := newFakeFTPServer(t, &fakeFTPServer{content: poolTestContent(1000)})
	u := "ftp://" + srv.addr() + "/f"

	readRange(t, u, 10, 8)
	ftpMetaEvict(time.Now().Add(ftpSizeTTL + time.Second))
	readRange(t, u, 20, 8)
	if got := srv.sizes.Load(); got != 2 {
		t.Errorf("SIZE commands = %d, want 2 (entry expired)", got)
	}
}

// TestOpenFTPStaleSessionRedials verifies a pooled session whose control
// connection died while idle (server idle timeout/restart) is detected on use
// and the open transparently succeeds on a fresh connection.
func TestOpenFTPStaleSessionRedials(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	resetFTPCaches(t)
	content := poolTestContent(1000)
	srv := newFakeFTPServer(t, &fakeFTPServer{content: content})
	u := "ftp://" + srv.addr() + "/f"

	readRange(t, u, 0, 8)
	if poolIdle() != 1 {
		t.Fatalf("idle = %d, want 1", poolIdle())
	}
	srv.dropControl()
	time.Sleep(50 * time.Millisecond) // let the FIN/RST arrive

	got, size := readRange(t, u, 100, 16)
	if !bytes.Equal(got, content[100:116]) || size != 1000 {
		t.Errorf("after stale session: got %q size %d", got, size)
	}
	if c, l := srv.conns.Load(), srv.logins.Load(); c != 2 || l != 2 {
		t.Errorf("control conns = %d, logins = %d; want 2 and 2 (one redial)", c, l)
	}
}

// TestOpenFTPAbortedTransferSessionReused verifies closing a stream before it
// ends (a seek abandoning the previous range) still leaves a reusable control
// connection: the server's 226/426 reply is consumed by Close.
func TestOpenFTPAbortedTransferSessionReused(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	resetFTPCaches(t)
	content := poolTestContent(8 << 20)
	srv := newFakeFTPServer(t, &fakeFTPServer{content: content})
	u := "ftp://" + srv.addr() + "/big"

	rc, _, err := openFTP(t.Context(), u, 0)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32)
	if _, err := io.ReadFull(rc, buf); err != nil {
		t.Fatal(err)
	}
	_ = rc.Close() // abandoned mid-stream
	if poolIdle() != 1 {
		t.Fatalf("idle after aborted transfer = %d, want 1", poolIdle())
	}

	got, _ := readRange(t, u, 1<<20, 16)
	if !bytes.Equal(got, content[1<<20:1<<20+16]) {
		t.Errorf("second open got %q", got)
	}
	if c := srv.conns.Load(); c != 1 {
		t.Errorf("control conns = %d, want 1 (session reused after abort)", c)
	}
}

// TestOpenFTPPoolBounded verifies concurrent opens each get their own session
// and that at most ftpPoolMaxPerKey stay idle afterwards; the rest are quit.
func TestOpenFTPPoolBounded(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	resetFTPCaches(t)
	srv := newFakeFTPServer(t, &fakeFTPServer{content: poolTestContent(1000)})
	u := "ftp://" + srv.addr() + "/f"

	const n = 6
	rcs := make([]io.ReadCloser, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			rc, _, err := openFTP(t.Context(), u, int64(i))
			if err != nil {
				t.Errorf("open %d: %v", i, err)
				return
			}
			rcs[i] = rc
		})
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	if c := srv.conns.Load(); c != n {
		t.Errorf("control conns while %d streams open = %d, want %d", n, c, n)
	}
	for _, rc := range rcs {
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
	}
	if got := poolIdle(); got != ftpPoolMaxPerKey {
		t.Errorf("idle after %d closes = %d, want cap %d", n, got, ftpPoolMaxPerKey)
	}
	// Overflow sessions are QUIT politely.
	deadline := time.Now().Add(5 * time.Second)
	for srv.quits.Load() < n-ftpPoolMaxPerKey && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if q := srv.quits.Load(); q != n-ftpPoolMaxPerKey {
		t.Errorf("QUIT count = %d, want %d", q, n-ftpPoolMaxPerKey)
	}
}

// TestOpenFTPPoolGlobalCapEvictsOldest verifies the pool-wide cap evicts the
// oldest idle session rather than refusing the newest one: stale sessions of
// servers nobody uses any more must not block pooling for the active one.
func TestOpenFTPPoolGlobalCapEvictsOldest(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	resetFTPCaches(t)
	srv := newFakeFTPServer(t, &fakeFTPServer{content: poolTestContent(100)})
	userURL := func(i int) string {
		return "ftp://u" + string(rune('a'+i)) + ":pw@" + srv.addr() + "/f"
	}

	const n = ftpPoolMaxIdle + 3
	for i := range n {
		readRange(t, userURL(i), 0, 8)
	}
	if got := poolIdle(); got != ftpPoolMaxIdle {
		t.Fatalf("idle = %d, want cap %d", got, ftpPoolMaxIdle)
	}
	if got := srv.logins.Load(); got != n {
		t.Fatalf("logins = %d, want %d", got, n)
	}
	readRange(t, userURL(n-1), 0, 8) // newest: still pooled
	if got := srv.logins.Load(); got != n {
		t.Errorf("logins after reusing newest = %d, want %d", got, n)
	}
	readRange(t, userURL(0), 0, 8) // oldest: was evicted
	if got := srv.logins.Load(); got != n+1 {
		t.Errorf("logins after oldest = %d, want %d (evicted, redial)", got, n+1)
	}
}

// TestOpenFTPPoolKeyedByCredentials verifies sessions are never shared across
// different credentials (a wrong password must not ride a pooled login).
func TestOpenFTPPoolKeyedByCredentials(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	resetFTPCaches(t)
	srv := newFakeFTPServer(t, &fakeFTPServer{content: poolTestContent(1000)})

	readRange(t, "ftp://alice:pw1@"+srv.addr()+"/f", 0, 8)
	readRange(t, "ftp://alice:pw2@"+srv.addr()+"/f", 0, 8)
	readRange(t, "ftp://bob:pw1@"+srv.addr()+"/f", 0, 8)
	if l := srv.logins.Load(); l != 3 {
		t.Errorf("logins = %d, want 3 (distinct credentials)", l)
	}
	readRange(t, "ftp://alice:pw1@"+srv.addr()+"/f", 0, 8)
	if l := srv.logins.Load(); l != 3 {
		t.Errorf("logins = %d, want 3 (same credentials reuse)", l)
	}
}

// TestOpenFTPPoolTTLEvicts verifies idle sessions older than the TTL are
// closed by the sweeper and not handed out.
func TestOpenFTPPoolTTLEvicts(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	resetFTPCaches(t)
	srv := newFakeFTPServer(t, &fakeFTPServer{content: poolTestContent(1000)})
	u := "ftp://" + srv.addr() + "/f"

	readRange(t, u, 0, 8)
	if poolIdle() != 1 {
		t.Fatalf("idle = %d, want 1", poolIdle())
	}
	ftpPoolEvict(time.Now()) // not yet expired
	if poolIdle() != 1 {
		t.Fatalf("idle after early sweep = %d, want 1", poolIdle())
	}
	ftpPoolEvict(time.Now().Add(ftpPoolTTL + time.Second))
	if poolIdle() != 0 {
		t.Fatalf("idle after TTL sweep = %d, want 0", poolIdle())
	}
	deadline := time.Now().Add(5 * time.Second)
	for srv.quits.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.quits.Load() != 1 {
		t.Errorf("QUIT count = %d, want 1", srv.quits.Load())
	}
	readRange(t, u, 0, 8)
	if c := srv.conns.Load(); c != 2 {
		t.Errorf("control conns = %d, want 2 after eviction", c)
	}
}

// TestOpenFTPCtxCancelNotPooled verifies a session whose request context was
// cancelled mid-transfer is torn down, never parked for reuse.
func TestOpenFTPCtxCancelNotPooled(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	resetFTPCaches(t)
	srv := newFakeFTPServer(t, &fakeFTPServer{content: []byte("abcdef"), stallData: true})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rc, _, err := openFTP(ctx, "ftp://"+srv.addr()+"/f", 0)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	if _, err := rc.Read(buf); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = rc.Close()
	if poolIdle() != 0 {
		t.Errorf("idle after cancelled transfer = %d, want 0", poolIdle())
	}
}

// TestOpenFTPCloseIdempotent verifies a double Close releases the session to
// the pool once — never twice (two owners of one ServerConn).
func TestOpenFTPCloseIdempotent(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	resetFTPCaches(t)
	srv := newFakeFTPServer(t, &fakeFTPServer{content: poolTestContent(100)})

	rc, _, err := openFTP(t.Context(), "ftp://"+srv.addr()+"/f", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()
	_ = rc.Close()
	if got := poolIdle(); got != 1 {
		t.Errorf("idle after double Close = %d, want 1", got)
	}
}

// TestOpenFTPPoolNotReusedAcrossPolicyFlip is the SSRF guard for pooling: a
// connection dialled while STREMIO_FTP_ALLOW_PRIVATE=1 must not be handed out
// once the opt-in is withdrawn — the open must be rejected like a fresh one.
func TestOpenFTPPoolNotReusedAcrossPolicyFlip(t *testing.T) {
	resetFTPCaches(t)
	srv := newFakeFTPServer(t, &fakeFTPServer{content: poolTestContent(100)})
	u := "ftp://" + srv.addr() + "/f"

	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	readRange(t, u, 0, 8)
	if poolIdle() != 1 {
		t.Fatalf("idle = %d, want 1", poolIdle())
	}

	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "")
	rc, _, err := openFTP(t.Context(), u, 0)
	if err == nil {
		_ = rc.Close()
		t.Fatal("loopback open succeeded after the private opt-in was withdrawn; pooled session leaked across policies")
	}
	if c := srv.conns.Load(); c != 1 {
		t.Errorf("control conns = %d, want 1 (no new dial either)", c)
	}
}

// TestOpenFTPDialStillGuarded verifies pooling did not weaken the per-dial
// Control hook: with the pre-flight cache primed to "allowed" for a blocked
// address, the dial itself still refuses it.
func TestOpenFTPDialStillGuarded(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "")
	resetFTPCaches(t)
	preflightPut(preflightKey{host: "127.0.0.1", blockPrivate: true}, nil) // pre-flight says "fine"

	_, _, err := openFTP(t.Context(), "ftp://127.0.0.1:1/secret", 0)
	if err == nil {
		t.Fatal("expected the dialer Control hook to reject a loopback dial")
	}
	if !strings.Contains(err.Error(), "dial") {
		t.Errorf("err = %v, want a dial-time rejection", err)
	}
	if poolIdle() != 0 {
		t.Errorf("idle = %d, want 0", poolIdle())
	}
}

// TestOpenRangedFTPUnknownSizeOpensOnce verifies that for an FTP server
// without SIZE, a ranged open falls back to byte 0 within a single login +
// RETR (no REST, no throwaway seeked transfer), where the old
// seek-then-reopen sequence cost two logins and two RETRs.
func TestOpenRangedFTPUnknownSizeOpensOnce(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	resetFTPCaches(t)
	content := poolTestContent(1000)
	srv := newFakeFTPServer(t, &fakeFTPServer{content: content, noSize: true})
	u := "ftp://" + srv.addr() + "/f"

	rc, size, pos, err := OpenRanged(t.Context(), u, 500)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if size != -1 || pos != 0 {
		t.Errorf("size, pos = %d, %d; want -1, 0", size, pos)
	}
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, content) {
		t.Errorf("body len %d, want full %d bytes from byte 0", len(got), len(content))
	}
	if l, r, rest := srv.logins.Load(), srv.retrs.Load(), srv.rests.Load(); l != 1 || r != 1 || rest != 0 {
		t.Errorf("logins=%d retr=%d rest=%d; want 1,1,0", l, r, rest)
	}
}

// TestOpenRangedFTPKnownSizeSeeks verifies the normal case is untouched:
// size known → reader positioned at offset, pos == offset.
func TestOpenRangedFTPKnownSizeSeeks(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	resetFTPCaches(t)
	content := poolTestContent(1000)
	srv := newFakeFTPServer(t, &fakeFTPServer{content: content})

	rc, size, pos, err := OpenRanged(t.Context(), "ftp://"+srv.addr()+"/f", 500)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if size != 1000 || pos != 500 {
		t.Errorf("size, pos = %d, %d; want 1000, 500", size, pos)
	}
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, content[500:]) {
		t.Errorf("body mismatch: len %d", len(got))
	}
}

// TestOpenFTPNoSizeKeepsSeekSemantics verifies plain Open keeps seeking even
// when SIZE is unavailable (size -1), as before.
func TestOpenFTPNoSizeKeepsSeekSemantics(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	resetFTPCaches(t)
	content := poolTestContent(1000)
	srv := newFakeFTPServer(t, &fakeFTPServer{content: content, noSize: true})

	rc, size, err := Open(t.Context(), "ftp://"+srv.addr()+"/f", 700)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if size != -1 || !bytes.Equal(got, content[700:]) {
		t.Errorf("size = %d, body len %d; want -1 and %d tail bytes", size, len(got), 300)
	}
}

// TestPreflightHostCached verifies the SSRF pre-flight verdict is cached per
// (host, policy): a primed entry is returned without any resolution (the
// ".invalid" TLD never resolves, which would otherwise yield nil), it expires,
// and the policy is part of the key.
func TestPreflightHostCached(t *testing.T) {
	resetFTPCaches(t)
	sentinel := errors.New("cached verdict")
	preflightPut(preflightKey{host: "cached.invalid", blockPrivate: true}, sentinel)

	if err := preflightHost(t.Context(), "cached.invalid", true); !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the cached verdict", err)
	}
	if err := preflightHost(t.Context(), "cached.invalid", false); err != nil {
		t.Errorf("other policy err = %v, want nil (separate key)", err)
	}
	ftpMetaEvict(time.Now().Add(preflightTTL + time.Second))
	if err := preflightHost(t.Context(), "cached.invalid", true); err != nil {
		t.Errorf("expired entry err = %v, want nil", err)
	}

	// A real verdict is stored on first call and reused.
	if err := preflightHost(t.Context(), "127.0.0.1", true); err == nil {
		t.Fatal("loopback must be blocked")
	}
	if hit, v := preflightGet(preflightKey{host: "127.0.0.1", blockPrivate: true}); !hit || v == nil {
		t.Errorf("verdict not cached: hit=%v err=%v", hit, v)
	}
	// Resolution failures are never cached.
	_ = preflightHost(t.Context(), "nxdomain.invalid", true)
	if hit, _ := preflightGet(preflightKey{host: "nxdomain.invalid", blockPrivate: true}); hit {
		t.Error("resolution failure must not be cached")
	}
}

// TestCachesBounded verifies the size and pre-flight caches never exceed their
// entry caps.
func TestCachesBounded(t *testing.T) {
	resetFTPCaches(t)
	for i := range ftpSizeMaxEntries * 2 {
		ftpSizePut(ftpSizeKey{addr: "h", user: "u", path: "/p" + string(rune('a'+i%26)) + strings.Repeat("x", i)}, 1)
	}
	for i := range preflightMaxEntries * 2 {
		preflightPut(preflightKey{host: strings.Repeat("h", i+1), blockPrivate: true}, nil)
	}
	ftpMetaMu.Lock()
	defer ftpMetaMu.Unlock()
	if len(ftpSizes) > ftpSizeMaxEntries {
		t.Errorf("size cache = %d entries, cap %d", len(ftpSizes), ftpSizeMaxEntries)
	}
	if len(preflights) > preflightMaxEntries {
		t.Errorf("preflight cache = %d entries, cap %d", len(preflights), preflightMaxEntries)
	}
}

// BenchmarkOpenFTPSeek times one ranged open+short read+close against a fake
// server that adds a 1ms "RTT" to every reply line, so the saved round-trips
// (dial, USER/PASS/FEAT/TYPE, SIZE) show up in ns/op. Before pooling: every
// op = 1 control conn + 1 login.
func BenchmarkOpenFTPSeek(b *testing.B) {
	b.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	resetFTPCaches(b)
	content := poolTestContent(10000)
	srv := newFakeFTPServer(b, &fakeFTPServer{content: content, delay: time.Millisecond})
	u := "ftp://" + srv.addr() + "/movie.mkv"

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		readRange(b, u, int64(1+i%9000), 64)
	}
	b.StopTimer()
	b.ReportMetric(float64(srv.logins.Load())/float64(b.N), "logins/op")
	b.ReportMetric(float64(srv.conns.Load())/float64(b.N), "ctlconns/op")
}

// BenchmarkPreflightHost compares the cached pre-flight with the resolution it
// replaces (the "uncached" sub-benchmark clears the cache each iteration,
// which is what every open used to pay).
func BenchmarkPreflightHost(b *testing.B) {
	for _, tc := range []struct {
		name   string
		bypass bool
	}{{"cached", false}, {"uncached", true}} {
		b.Run(tc.name, func(b *testing.B) {
			resetFTPCaches(b)
			b.ReportAllocs()
			for b.Loop() {
				if tc.bypass {
					ftpMetaMu.Lock()
					clear(preflights)
					ftpMetaMu.Unlock()
				}
				if err := preflightHost(b.Context(), "localhost", false); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
