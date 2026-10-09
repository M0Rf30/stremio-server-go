// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package streamproxy

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// allocatedBytes returns the bytes the runtime allocated while f ran.
func allocatedBytes(f func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// ---------------------------------------------------------------------------
// byteBudget
// ---------------------------------------------------------------------------

func TestByteBudgetTryAcquireRelease(t *testing.T) {
	b := newByteBudget(10)
	if !b.tryAcquire(6) {
		t.Fatal("6/10 must fit")
	}
	if b.tryAcquire(5) {
		t.Fatal("5 more must not fit (11/10)")
	}
	if !b.tryAcquire(4) {
		t.Fatal("4 more must fit (10/10)")
	}
	b.release(6)
	if !b.tryAcquire(5) {
		t.Fatal("5 must fit after releasing 6")
	}
	if b.peak != 10 {
		t.Errorf("peak=%d want 10", b.peak)
	}
	b.release(4)
	b.release(5)
	if b.used != 0 {
		t.Errorf("used=%d want 0", b.used)
	}
}

func TestByteBudgetNilIsUnlimited(t *testing.T) {
	var b *byteBudget
	if !b.tryAcquire(1 << 40) {
		t.Error("nil budget must admit everything")
	}
	if err := b.acquire(t.Context(), 1<<40); err != nil {
		t.Errorf("nil acquire: %v", err)
	}
	b.release(1 << 40) // must not panic
}

func TestByteBudgetOversizeRejected(t *testing.T) {
	b := newByteBudget(10)
	if b.tryAcquire(11) {
		t.Error("tryAcquire above the whole budget must fail")
	}
	if err := b.acquire(t.Context(), 11); !errors.Is(err, errBudgetTooBig) {
		t.Errorf("acquire(11) = %v, want errBudgetTooBig", err)
	}
}

// waitForWaiters blocks until n acquirers are queued on b.
func waitForWaiters(t *testing.T, b *byteBudget, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		b.mu.Lock()
		got := b.waiters.Len()
		b.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiters=%d, want %d", got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestByteBudgetAcquireBlocksUntilRelease(t *testing.T) {
	b := newByteBudget(10)
	b.tryAcquire(10)
	done := make(chan error, 1)
	go func() { done <- b.acquire(t.Context(), 4) }()
	waitForWaiters(t, b, 1)
	select {
	case <-done:
		t.Fatal("acquire returned while the budget was full")
	case <-time.After(20 * time.Millisecond):
	}
	b.release(4)
	if err := <-done; err != nil {
		t.Fatalf("acquire: %v", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used != 10 {
		t.Errorf("used=%d want 10", b.used)
	}
}

// Waiters are served FIFO: a small request must not overtake a large one
// queued before it (which would starve the large one indefinitely).
func TestByteBudgetFIFO(t *testing.T) {
	b := newByteBudget(10)
	b.tryAcquire(10)
	start := func(n int64) chan struct{} {
		done := make(chan struct{})
		go func() {
			if err := b.acquire(t.Context(), n); err != nil {
				t.Errorf("acquire(%d): %v", n, err)
			}
			close(done)
		}()
		return done
	}
	big := start(8)
	waitForWaiters(t, b, 1)
	small := start(2)
	waitForWaiters(t, b, 2)

	// 2 bytes free: enough for "small", but "big" is ahead of it.
	b.release(2)
	select {
	case <-small:
		t.Fatal("a waiter overtook the queue head")
	case <-big:
		t.Fatal("head admitted without enough room")
	case <-time.After(20 * time.Millisecond):
	}

	b.release(8) // 10 free: head fits, then the follower behind it
	for name, ch := range map[string]chan struct{}{"big": big, "small": small} {
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s was never admitted", name)
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used != 10 {
		t.Errorf("used=%d want 10", b.used)
	}
}

func TestByteBudgetCancelledHeadUnblocksSuccessors(t *testing.T) {
	b := newByteBudget(10)
	b.tryAcquire(10)
	b.release(5) // 5 free, head wants 8, follower wants 1

	ctx, cancel := context.WithCancel(t.Context())
	headErr := make(chan error, 1)
	go func() { headErr <- b.acquire(ctx, 8) }()
	waitForWaiters(t, b, 1)
	tailErr := make(chan error, 1)
	go func() { tailErr <- b.acquire(t.Context(), 1) }()
	waitForWaiters(t, b, 2)

	cancel()
	if err := <-headErr; !errors.Is(err, context.Canceled) {
		t.Errorf("head: %v, want context.Canceled", err)
	}
	if err := <-tailErr; err != nil {
		t.Errorf("follower must be admitted once the head leaves: %v", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used != 6 || b.waiters.Len() != 0 {
		t.Errorf("used=%d waiters=%d, want 6 and 0", b.used, b.waiters.Len())
	}
}

func TestByteBudgetConcurrentNeverExceedsMax(t *testing.T) {
	const limit = 8
	b := newByteBudget(limit)
	var inUse, over atomic.Int64
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				n := int64((g+i)%4 + 1)
				if i%3 == 0 {
					if !b.tryAcquire(n) {
						continue
					}
				} else if err := b.acquire(t.Context(), n); err != nil {
					t.Errorf("acquire: %v", err)
					return
				}
				if inUse.Add(n) > limit {
					over.Add(1)
				}
				inUse.Add(-n)
				b.release(n)
			}
		}()
	}
	wg.Wait()
	if over.Load() != 0 {
		t.Errorf("budget exceeded %d times", over.Load())
	}
	if b.used != 0 || b.peak > limit {
		t.Errorf("used=%d peak=%d (limit %d)", b.used, b.peak, limit)
	}
}

// ---------------------------------------------------------------------------
// readSegment
// ---------------------------------------------------------------------------

// failReader fails the test if anything tries to read from it.
type failReader struct{ t *testing.T }

func (f failReader) Read([]byte) (int, error) {
	f.t.Error("body must not be read")
	return 0, io.EOF
}

func TestReadSegment(t *testing.T) {
	payload := bytes.Repeat([]byte("segment!"), 100)
	t.Run("known length", func(t *testing.T) {
		got, err := readSegment(bytes.NewReader(payload), int64(len(payload)))
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("got %d bytes, err=%v", len(got), err)
		}
		if cap(got) != len(payload) {
			t.Errorf("cap=%d want exactly %d", cap(got), len(payload))
		}
	})
	t.Run("zero length", func(t *testing.T) {
		got, err := readSegment(bytes.NewReader(nil), 0)
		if err != nil || len(got) != 0 {
			t.Fatalf("got %d bytes, err=%v", len(got), err)
		}
	})
	t.Run("unknown length", func(t *testing.T) {
		got, err := readSegment(bytes.NewReader(payload), -1)
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("got %d bytes, err=%v", len(got), err)
		}
	})
	t.Run("advertised oversize is refused unread", func(t *testing.T) {
		if _, err := readSegment(failReader{t}, maxSegmentBytes+1); !errors.Is(err, errSegmentTooLarge) {
			t.Fatalf("err=%v, want errSegmentTooLarge", err)
		}
	})
	t.Run("short body is an error, not a truncated segment", func(t *testing.T) {
		got, err := readSegment(bytes.NewReader(payload[:10]), int64(len(payload)))
		if !errors.Is(err, io.ErrUnexpectedEOF) || got != nil {
			t.Fatalf("got %d bytes, err=%v; want io.ErrUnexpectedEOF", len(got), err)
		}
	})
}

// A known-length body must cost one buffer, not the ~2x of append growth.
func TestReadSegmentAllocatesExactSize(t *testing.T) {
	const size = 4 << 20
	src := bytes.NewReader(make([]byte, size))
	var err error
	got := allocatedBytes(func() { _, err = readSegment(src, size) })
	if err != nil {
		t.Fatal(err)
	}
	if got > size+size/16 {
		t.Errorf("allocated %d bytes for a %d-byte segment, want ~1x", got, size)
	}
}

// ---------------------------------------------------------------------------
// segCache byte budget
// ---------------------------------------------------------------------------

func TestSegCachePutFullOversizeNotStored(t *testing.T) {
	c := newSegCache(time.Minute, 10)
	c.maxBytes = 10
	c.putFull("a", []byte("1234"), nil, 200)
	c.putFull("b", []byte("5678"), nil, 200)
	c.putFull("big", make([]byte, 11), nil, 200)
	if c.getFull("big") != nil {
		t.Error("oversize entry must not be stored")
	}
	if c.getFull("a") == nil || c.getFull("b") == nil {
		t.Error("oversize put must not flush existing entries")
	}
	if c.totalBytes != 8 {
		t.Errorf("totalBytes=%d want 8", c.totalBytes)
	}
}

func TestSegCachePutFullOversizeDropsStaleCopy(t *testing.T) {
	c := newSegCache(time.Minute, 10)
	c.maxBytes = 10
	c.putFull("k", []byte("1234"), nil, 200)
	c.putFull("k", make([]byte, 11), nil, 200)
	if c.getFull("k") != nil {
		t.Error("stale copy must not outlive an oversize replacement")
	}
	if c.totalBytes != 0 || c.lru.Len() != 0 {
		t.Errorf("bookkeeping off: bytes=%d len=%d", c.totalBytes, c.lru.Len())
	}
}

func TestSegCacheUpdateToLargerEvictsToStayInBudget(t *testing.T) {
	c := newSegCache(time.Minute, 10)
	c.maxBytes = 10
	c.putFull("a", []byte("123"), nil, 200)
	c.putFull("b", []byte("456"), nil, 200)
	c.putFull("a", []byte("12345678"), nil, 200) // 8 + 3 > 10 → "b" (cold end) goes
	if c.getFull("b") != nil {
		t.Error("cold entry must be evicted to honour the byte budget")
	}
	if e := c.getFull("a"); e == nil || len(e.val) != 8 {
		t.Error("refreshed entry must survive its own update")
	}
	if c.totalBytes > c.maxBytes {
		t.Errorf("totalBytes=%d exceeds budget %d", c.totalBytes, c.maxBytes)
	}
}

// ---------------------------------------------------------------------------
// In-place decryption
// ---------------------------------------------------------------------------

func drmTestCBCSegment(t *testing.T, key, iv []byte, size int) (plain, ct []byte) {
	t.Helper()
	plain = drmTestPlain(size, 0x3c)
	blk, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	padded := drmPKCS7Pad(plain, aes.BlockSize)
	ct = make([]byte, len(padded))
	cipher.NewCBCEncrypter(blk, iv).CryptBlocks(ct, padded)
	return plain, ct
}

func TestDrmDecryptInPlaceCBC(t *testing.T) {
	key, iv := []byte("0123456789abcdef"), []byte("fedcba9876543210")
	plain, ct := drmTestCBCSegment(t, key, iv, 64*1024+5)
	p := DecryptParams{Method: "AES-128", Key: key, IV: iv}

	// drmDecrypt leaves the ciphertext alone.
	orig := append([]byte(nil), ct...)
	got, err := drmDecrypt(nil, p, ct)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("drmDecrypt: err=%v equal=%v", err, bytes.Equal(got, plain))
	}
	if !bytes.Equal(ct, orig) {
		t.Error("drmDecrypt modified its input")
	}

	// drmDecryptInPlace reuses it.
	got, err = drmDecryptInPlace(nil, p, ct)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("drmDecryptInPlace: err=%v equal=%v", err, bytes.Equal(got, plain))
	}
	if &got[0] != &ct[0] {
		t.Error("drmDecryptInPlace allocated a new buffer")
	}
}

func TestDrmDecryptInPlaceCBCS(t *testing.T) {
	key := drmTestPlain(16, 0x51)
	iv := drmTestPlain(16, 0x62)
	subs := [][]drmSubsample{{{Clear: 20, Encrypted: 16*300 + 11}}}
	plain := drmTestPlain(20+16*300+11, 0x03)
	body := drmTestCBCSProtect(t, key, iv, plain, subs[0], 1, 9)
	seg := drmTestBuildMediaSegment(drmTestSubsampleSenc(0x2, 0, subs), []int{len(plain)}, body)
	p := DecryptParams{Method: "CBCS", Key: key, IV: iv}

	orig := append([]byte(nil), seg...)
	want, err := drmDecrypt(nil, p, seg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(seg, orig) {
		t.Error("drmDecrypt modified its input")
	}
	if got := drmTestMdatBody(t, want); !bytes.Equal(got, plain) {
		t.Fatal("reference decrypt did not recover the plaintext")
	}

	got, err := drmDecryptInPlace(nil, p, seg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("in-place CBCS output differs from the copying path")
	}
	if &got[0] != &seg[0] {
		t.Error("drmDecryptInPlace allocated a new buffer")
	}
}

// The point of in-place decryption: no second full-size buffer.
func TestDrmDecryptInPlaceAllocatesNoCopy(t *testing.T) {
	key, iv := []byte("0123456789abcdef"), []byte("fedcba9876543210")
	_, ct := drmTestCBCSegment(t, key, iv, 4<<20)
	p := DecryptParams{Method: "AES-128", Key: key, IV: iv}

	copyCT := append([]byte(nil), ct...)
	var err error
	inPlace := allocatedBytes(func() { _, err = drmDecryptInPlace(nil, p, copyCT) })
	if err != nil {
		t.Fatal(err)
	}
	copying := allocatedBytes(func() { _, err = drmDecrypt(nil, p, ct) })
	if err != nil {
		t.Fatal(err)
	}
	if inPlace > 64<<10 {
		t.Errorf("in-place decrypt allocated %d bytes, want ~0", inPlace)
	}
	if copying < uint64(len(ct)) {
		t.Errorf("copying decrypt allocated %d bytes, want >= %d (sanity)", copying, len(ct))
	}
}

// ---------------------------------------------------------------------------
// Handler-level: in-flight accounting
// ---------------------------------------------------------------------------

// gatedServer serves body (Content-Length known) but parks every handler
// after the first chunk until n requests have arrived, so the proxy sees all
// n fetches in flight before any body completes.
func gatedServer(t *testing.T, n int, body []byte) *httptest.Server {
	t.Helper()
	var arrived atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		const first = 4 << 10
		_, _ = w.Write(body[:first])
		w.(http.Flusher).Flush()
		arrived.Add(1)
		for deadline := time.Now().Add(5 * time.Second); int(arrived.Load()) < n && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
		_, _ = w.Write(body[first:])
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCachedFetchInflightBounded(t *testing.T) {
	const (
		seg   = 256 << 10
		n     = 12
		limit = 3 * seg
	)
	body := bytes.Repeat([]byte{7}, seg)
	srv := gatedServer(t, n, body)
	h := New(Config{SegCacheTTL: time.Minute, Client: srv.Client()})
	t.Cleanup(h.Close)
	h.inflight = newByteBudget(limit)

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, _, status, err := h.cachedFetch(t.Context(), srv.URL+"/seg"+strconv.Itoa(i)+".ts", nil, "")
			if err != nil || status != http.StatusOK || !bytes.Equal(got, body) {
				t.Errorf("fetch %d: status=%d len=%d err=%v", i, status, len(got), err)
			}
		}()
	}
	wg.Wait()

	h.inflight.mu.Lock()
	defer h.inflight.mu.Unlock()
	if h.inflight.peak > limit {
		t.Errorf("in-flight peak %d exceeds budget %d", h.inflight.peak, limit)
	}
	if h.inflight.peak < seg {
		t.Errorf("in-flight peak %d: budget was never used", h.inflight.peak)
	}
	if h.inflight.used != 0 {
		t.Errorf("reservation leaked: used=%d", h.inflight.used)
	}
}

// Prefetch is best-effort: with the budget full it gives up rather than
// queueing behind real client requests, and caches nothing.
func TestPrefetchSkipsWhenInflightBudgetBusy(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Length", "5")
		_, _ = io.WriteString(w, "hello")
	}))
	defer srv.Close()
	h := New(Config{SegCacheTTL: time.Minute, Client: srv.Client()})
	t.Cleanup(h.Close)
	h.inflight = newByteBudget(1 << 20)
	if !h.inflight.tryAcquire(1 << 20) {
		t.Fatal("setup")
	}

	_, _, _, err := h.cachedFetchMode(t.Context(), srv.URL+"/s.ts", nil, "", false)
	if !errors.Is(err, errInflightBusy) {
		t.Fatalf("err=%v, want errInflightBusy", err)
	}
	if entries, _ := h.CacheStats(); entries != 0 {
		t.Errorf("entries=%d, want 0", entries)
	}
	if len(h.flights) != 0 {
		t.Errorf("flight leaked: %d", len(h.flights))
	}

	h.inflight.release(1 << 20)
	got, _, _, err := h.cachedFetchMode(t.Context(), srv.URL+"/s.ts", nil, "", false)
	if err != nil || string(got) != "hello" {
		t.Fatalf("after release: %q, %v", got, err)
	}
	if entries, _ := h.CacheStats(); entries != 1 {
		t.Errorf("entries=%d, want 1", entries)
	}
}

// A Content-Length over the ceiling is refused without reading the body.
func TestCachedFetchAdvertisedTooLargeRefusedUnread(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.FormatInt(maxSegmentBytes+1, 10))
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // never sends a body byte
	}))
	defer srv.Close()
	for _, cached := range []bool{true, false} {
		cfg := Config{Client: srv.Client()}
		if cached {
			cfg.SegCacheTTL = time.Minute
		}
		h := New(cfg)
		type result struct {
			status int
			err    error
		}
		done := make(chan result, 1)
		go func() {
			_, _, status, err := h.cachedFetch(t.Context(), srv.URL+"/big.ts", nil, "")
			done <- result{status, err}
		}()
		select {
		case r := <-done:
			if r.err == nil || r.status != http.StatusBadGateway {
				t.Errorf("cached=%v: status=%d err=%v, want 502 + error", cached, r.status, r.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("cached=%v: oversize Content-Length was not refused before reading the body", cached)
		}
		h.Close()
	}
}

func newStreamProxyForTest(t *testing.T, srv *httptest.Server, ttl time.Duration) *Handler {
	t.Helper()
	h := New(Config{SegCacheTTL: ttl, Client: srv.Client(), PublicURL: "https://ext.example"})
	t.Cleanup(h.Close)
	return h
}

func TestServeStreamBudgetBusyStreamsUncached(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Length", "5")
		_, _ = io.WriteString(w, "hello")
	}))
	defer srv.Close()
	h := newStreamProxyForTest(t, srv, time.Minute)
	h.inflight = newByteBudget(1 << 20)
	h.inflight.tryAcquire(1 << 20) // budget exhausted

	get := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.serveStream(w, httptest.NewRequest("GET", "/proxy/stream?d="+url.QueryEscape(srv.URL+"/s.ts"), nil))
		return w
	}
	if w := get(); w.Code != http.StatusOK || w.Body.String() != "hello" {
		t.Fatalf("busy: %d %q", w.Code, w.Body.String())
	}
	if entries, _ := h.CacheStats(); entries != 0 {
		t.Errorf("busy budget must not buffer/cache, entries=%d", entries)
	}

	h.inflight.release(1 << 20)
	if w := get(); w.Code != http.StatusOK || w.Body.String() != "hello" {
		t.Fatalf("free: %d %q", w.Code, w.Body.String())
	}
	if entries, _ := h.CacheStats(); entries != 1 {
		t.Errorf("entries=%d, want 1 once the budget is free", entries)
	}
	if h.inflight.used != 0 {
		t.Errorf("reservation leaked: used=%d", h.inflight.used)
	}
}

// A response bigger than the whole cache budget would never be kept, so it
// must be streamed instead of buffered.
func TestServeStreamLargerThanCacheBudgetStreams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "5")
		_, _ = io.WriteString(w, "hello")
	}))
	defer srv.Close()
	h := newStreamProxyForTest(t, srv, time.Minute)
	h.cache.maxBytes = 3

	w := httptest.NewRecorder()
	h.serveStream(w, httptest.NewRequest("GET", "/proxy/stream?d="+url.QueryEscape(srv.URL+"/s.ts"), nil))
	if w.Code != http.StatusOK || w.Body.String() != "hello" {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
	if entries, _ := h.CacheStats(); entries != 0 {
		t.Errorf("entries=%d, want 0", entries)
	}
	h.inflight.mu.Lock()
	defer h.inflight.mu.Unlock()
	if h.inflight.peak != 0 {
		t.Errorf("a response that cannot be cached must not be buffered (peak=%d)", h.inflight.peak)
	}
}

// hookWriter runs onWrite before the body is written, i.e. while serveStream
// still holds its in-flight reservation.
type hookWriter struct {
	*httptest.ResponseRecorder
	onWrite func()
}

func (w hookWriter) Write(p []byte) (int, error) {
	if w.onWrite != nil {
		w.onWrite()
	}
	return w.ResponseRecorder.Write(p)
}

func decryptTarget(dest string, key, iv []byte) string {
	return "/proxy/stream?d=" + url.QueryEscape(dest) +
		"&method=AES-128&key=" + hex.EncodeToString(key) + "&iv=" + hex.EncodeToString(iv)
}

func TestServeStreamDecryptInPlaceRoundTrip(t *testing.T) {
	key, iv := []byte("0123456789abcdef"), []byte("fedcba9876543210")
	plain, ct := drmTestCBCSegment(t, key, iv, 200*1024+3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Content-Length", strconv.Itoa(len(ct)))
		_, _ = w.Write(ct)
	}))
	defer srv.Close()
	h := newStreamProxyForTest(t, srv, 0)

	var heldDuringWrite int64
	w := hookWriter{ResponseRecorder: httptest.NewRecorder()}
	w.onWrite = func() {
		h.inflight.mu.Lock()
		heldDuringWrite = h.inflight.used
		h.inflight.mu.Unlock()
	}
	h.serveStream(w, httptest.NewRequest("GET", decryptTarget(srv.URL+"/s.ts", key, iv), nil))
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), plain) {
		t.Fatalf("status=%d body ok=%v", w.Code, bytes.Equal(w.Body.Bytes(), plain))
	}
	if got := w.Header().Get("Content-Length"); got != strconv.Itoa(len(plain)) {
		t.Errorf("Content-Length=%q want %d", got, len(plain))
	}
	if heldDuringWrite != int64(len(ct)) {
		t.Errorf("held %d bytes while writing, want the segment size %d", heldDuringWrite, len(ct))
	}
	if h.inflight.used != 0 {
		t.Errorf("reservation leaked: used=%d", h.inflight.used)
	}
}

// An upstream with no Content-Length reserves the worst case up front, then
// shrinks to what it actually sent so it does not starve other requests.
func TestServeStreamDecryptChunkedShrinksReservation(t *testing.T) {
	key, iv := []byte("0123456789abcdef"), []byte("fedcba9876543210")
	plain, ct := drmTestCBCSegment(t, key, iv, 5000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush() // force chunked: no Content-Length
		_, _ = w.Write(ct)
	}))
	defer srv.Close()
	h := newStreamProxyForTest(t, srv, 0)

	var heldDuringWrite int64
	w := hookWriter{ResponseRecorder: httptest.NewRecorder()}
	w.onWrite = func() {
		h.inflight.mu.Lock()
		heldDuringWrite = h.inflight.used
		h.inflight.mu.Unlock()
	}
	h.serveStream(w, httptest.NewRequest("GET", decryptTarget(srv.URL+"/s.ts", key, iv), nil))
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), plain) {
		t.Fatalf("status=%d body ok=%v", w.Code, bytes.Equal(w.Body.Bytes(), plain))
	}
	if heldDuringWrite != int64(len(ct)) {
		t.Errorf("held %d bytes while writing, want %d (actual size, not the %d worst case)", heldDuringWrite, len(ct), maxSegmentBytes)
	}
	h.inflight.mu.Lock()
	defer h.inflight.mu.Unlock()
	if h.inflight.peak != maxSegmentBytes {
		t.Errorf("peak=%d, want the worst-case reservation %d", h.inflight.peak, maxSegmentBytes)
	}
	if h.inflight.used != 0 {
		t.Errorf("reservation leaked: used=%d", h.inflight.used)
	}
}

func TestServeStreamDecryptWaitsForBudget(t *testing.T) {
	key, iv := []byte("0123456789abcdef"), []byte("fedcba9876543210")
	plain, ct := drmTestCBCSegment(t, key, iv, 4000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(ct)))
		_, _ = w.Write(ct)
	}))
	defer srv.Close()
	h := newStreamProxyForTest(t, srv, 0)
	h.inflight = newByteBudget(int64(len(ct)))
	h.inflight.tryAcquire(int64(len(ct))) // somebody else holds everything

	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.serveStream(w, httptest.NewRequest("GET", decryptTarget(srv.URL+"/s.ts", key, iv), nil))
	}()
	waitForWaiters(t, h.inflight, 1)
	select {
	case <-done:
		t.Fatal("decrypt ran while the budget was exhausted")
	case <-time.After(20 * time.Millisecond):
	}
	h.inflight.release(int64(len(ct)))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("decrypt did not resume after the budget freed")
	}
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), plain) {
		t.Errorf("status=%d body ok=%v", w.Code, bytes.Equal(w.Body.Bytes(), plain))
	}
}

func TestServeStreamDecryptBusyAnswers503(t *testing.T) {
	key, iv := []byte("0123456789abcdef"), []byte("fedcba9876543210")
	_, ct := drmTestCBCSegment(t, key, iv, 4000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(ct)))
		_, _ = w.Write(ct)
	}))
	defer srv.Close()
	h := newStreamProxyForTest(t, srv, 0)
	h.inflight = newByteBudget(int64(len(ct)))
	h.inflight.tryAcquire(int64(len(ct)))

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest("GET", decryptTarget(srv.URL+"/s.ts", key, iv), nil).WithContext(ctx)
	w := httptest.NewRecorder()
	h.serveStream(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503", w.Code)
	}
	h.inflight.mu.Lock()
	defer h.inflight.mu.Unlock()
	if h.inflight.used != int64(len(ct)) || h.inflight.waiters.Len() != 0 {
		t.Errorf("used=%d waiters=%d: the abandoned request must leave the budget untouched", h.inflight.used, h.inflight.waiters.Len())
	}
}

func TestServeStreamDecryptAdvertisedTooLargeRefusedUnread(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.FormatInt(maxSegmentBytes+1, 10))
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	h := newStreamProxyForTest(t, srv, 0)

	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.serveStream(w, httptest.NewRequest("GET", decryptTarget(srv.URL+"/s.ts", make([]byte, 16), make([]byte, 16)), nil))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("oversize Content-Length was not refused before reading the body")
	}
	if w.Code != http.StatusBadGateway {
		t.Errorf("status=%d want 502", w.Code)
	}
}
