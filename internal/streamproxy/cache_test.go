package streamproxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// segCache unit tests
// ---------------------------------------------------------------------------

func TestSegCachePutAndGet(t *testing.T) {
	c := newSegCache(time.Minute, 10)
	hdr := http.Header{"Content-Type": []string{"video/mp2t"}}
	c.putFull("seg1", []byte("hello"), hdr, 200)

	e := c.getFull("seg1")
	if e == nil {
		t.Fatal("getFull returned nil for a stored entry")
	}
	if !bytes.Equal(e.val, []byte("hello")) {
		t.Errorf("val mismatch: got %q want %q", e.val, "hello")
	}
	if e.status != 200 {
		t.Errorf("status: got %d want 200", e.status)
	}
	if e.hdr.Get("Content-Type") != "video/mp2t" {
		t.Errorf("header mismatch: got %q", e.hdr.Get("Content-Type"))
	}
}

func TestSegCacheMiss(t *testing.T) {
	c := newSegCache(time.Minute, 10)
	if e := c.getFull("nonexistent"); e != nil {
		t.Errorf("expected nil for missing key, got %+v", e)
	}
}

func TestSegCacheTTLExpiry(t *testing.T) {
	c := newSegCache(5*time.Millisecond, 10)
	c.putFull("k1", []byte("data"), nil, 200)

	// Entry must be present immediately after put.
	if e := c.getFull("k1"); e == nil {
		t.Fatal("entry missing immediately after put")
	}

	// After the TTL has elapsed the entry must be absent.
	time.Sleep(20 * time.Millisecond)
	if e := c.getFull("k1"); e != nil {
		t.Error("entry still present after TTL expiry")
	}
}

func TestSegCacheLRUEviction(t *testing.T) {
	c := newSegCache(time.Minute, 2)
	c.putFull("a", []byte("aaa"), nil, 200)
	c.putFull("b", []byte("bbb"), nil, 200)
	// Access "a" — it becomes the MRU; "b" is now LRU.
	_ = c.getFull("a")
	// Adding "c" must evict the LRU entry "b".
	c.putFull("c", []byte("ccc"), nil, 200)

	if e := c.getFull("b"); e != nil {
		t.Error("expected 'b' evicted as LRU, but still present")
	}
	if e := c.getFull("a"); e == nil {
		t.Error("expected 'a' to survive (recently accessed)")
	}
	if e := c.getFull("c"); e == nil {
		t.Error("expected 'c' to be present")
	}
}

func TestSegCacheByteCapEviction(t *testing.T) {
	c := newSegCache(time.Minute, 1000)
	c.maxBytes = 10 // very tight byte budget

	// 6 bytes fits.
	c.putFull("k1", []byte("123456"), nil, 200)
	// Adding 6 more bytes (total 12 > 10) must evict k1.
	c.putFull("k2", []byte("abcdef"), nil, 200)

	if e := c.getFull("k1"); e != nil {
		t.Error("expected k1 evicted to stay within byte cap")
	}
	if e := c.getFull("k2"); e == nil {
		t.Error("expected k2 present after eviction")
	}
}

func TestSegCacheUpdateExisting(t *testing.T) {
	c := newSegCache(time.Minute, 2)
	c.putFull("k", []byte("v1"), nil, 200)
	c.putFull("k", []byte("v2_updated"), nil, 201)

	e := c.getFull("k")
	if e == nil {
		t.Fatal("entry missing after update")
	}
	if string(e.val) != "v2_updated" {
		t.Errorf("val not updated: got %q", e.val)
	}
	if e.status != 201 {
		t.Errorf("status not updated: got %d", e.status)
	}
}

func TestSegCacheTotalBytesTracked(t *testing.T) {
	c := newSegCache(time.Minute, 100)
	c.putFull("a", []byte("abc"), nil, 200)   // 3 bytes
	c.putFull("b", []byte("defgh"), nil, 200) // 5 bytes

	c.mu.Lock()
	total := c.totalBytes
	c.mu.Unlock()

	if total != 8 {
		t.Errorf("totalBytes: got %d want 8", total)
	}
}

func TestSegCacheTotalBytesOnEvict(t *testing.T) {
	c := newSegCache(time.Minute, 1)        // cap of 1 entry
	c.putFull("a", []byte("abc"), nil, 200) // 3 bytes

	c.mu.Lock()
	before := c.totalBytes
	c.mu.Unlock()

	if before != 3 {
		t.Errorf("before eviction totalBytes=%d want 3", before)
	}

	c.putFull("b", []byte("xy"), nil, 200) // evicts "a"

	c.mu.Lock()
	after := c.totalBytes
	c.mu.Unlock()

	if after != 2 {
		t.Errorf("after eviction totalBytes=%d want 2", after)
	}
}

// ---------------------------------------------------------------------------
// CacheStats
// ---------------------------------------------------------------------------

func TestCacheStatsNoCache(t *testing.T) {
	h := New(Config{}) // SegCacheTTL==0 → no cache created
	entries, b := h.CacheStats()
	if entries != 0 || b != 0 {
		t.Errorf("CacheStats with no cache: got (%d, %d) want (0, 0)", entries, b)
	}
}

func TestCacheStatsWithEntries(t *testing.T) {
	h := New(Config{SegCacheTTL: time.Minute})
	data := []byte("segment-data")
	h.cache.putFull("url1", data, nil, 200)
	h.cache.putFull("url2", []byte("x"), nil, 200)

	entries, b := h.CacheStats()
	if entries != 2 {
		t.Errorf("entries: got %d want 2", entries)
	}
	wantBytes := int64(len(data) + 1)
	if b != wantBytes {
		t.Errorf("bytes: got %d want %d", b, wantBytes)
	}
}

// ---------------------------------------------------------------------------
// cachedFetch — via httptest upstream
// ---------------------------------------------------------------------------

func TestCachedFetchCacheMissAndHit(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "video/mp2t")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("segment-body"))
	}))
	defer srv.Close()

	h := New(Config{SegCacheTTL: time.Minute, Client: srv.Client()})
	ctx := t.Context()

	// First call: cache miss → upstream request.
	body, hdr, status, err := h.cachedFetch(ctx, srv.URL+"/seg.ts", nil, "")
	if err != nil {
		t.Fatalf("first cachedFetch: %v", err)
	}
	if status != 200 {
		t.Errorf("first status: got %d want 200", status)
	}
	if string(body) != "segment-body" {
		t.Errorf("first body: got %q", body)
	}
	if calls != 1 {
		t.Errorf("upstream calls after miss: got %d want 1", calls)
	}
	_ = hdr // headers forwarded

	// Second call: cache hit → no upstream request.
	body2, _, status2, err := h.cachedFetch(ctx, srv.URL+"/seg.ts", nil, "")
	if err != nil {
		t.Fatalf("second cachedFetch: %v", err)
	}
	if status2 != 200 {
		t.Errorf("second status: got %d want 200", status2)
	}
	if string(body2) != "segment-body" {
		t.Errorf("second body: got %q", body2)
	}
	if calls != 1 {
		t.Errorf("cache hit still hit upstream: calls=%d want 1", calls)
	}

	// Cache must have exactly 1 entry.
	entries, _ := h.CacheStats()
	if entries != 1 {
		t.Errorf("cache entries: got %d want 1", entries)
	}
}

func TestCachedFetchNoCaching(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(200)
		_, _ = w.Write([]byte("body"))
	}))
	defer srv.Close()

	h := New(Config{Client: srv.Client()}) // SegCacheTTL==0 → no cache
	ctx := t.Context()

	for i := 0; i < 3; i++ {
		_, _, _, err := h.cachedFetch(ctx, srv.URL+"/seg.ts", nil, "")
		if err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	if calls != 3 {
		t.Errorf("without cache: expected 3 upstream calls, got %d", calls)
	}
}

func TestCachedFetchNon200NotCached(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	h := New(Config{SegCacheTTL: time.Minute, Client: srv.Client()})
	ctx := t.Context()

	_, _, status, err := h.cachedFetch(ctx, srv.URL+"/missing.ts", nil, "")
	if err != nil {
		t.Fatalf("cachedFetch: %v", err)
	}
	if status != 404 {
		t.Errorf("status: got %d want 404", status)
	}
	// 404 must not be stored in the cache.
	entries, _ := h.CacheStats()
	if entries != 0 {
		t.Errorf("404 response must not be cached, got %d entries", entries)
	}
}

// zeroReader is an infinite source of zero bytes used to synthesise large responses
// in tests without pre-allocating a large slice.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// TestCachedFetchTooLargeCachingPath verifies that a segment larger than
// maxSegmentBytes is rejected and not stored, on the caching code path.
// This test allocates ~50 MiB of request body data; it is intentionally slow.
func TestCachedFetchTooLargeCachingPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.CopyN(w, zeroReader{}, maxSegmentBytes+1)
	}))
	defer srv.Close()

	h := New(Config{SegCacheTTL: time.Minute, Client: srv.Client()})
	ctx := t.Context()

	_, _, status, err := h.cachedFetch(ctx, srv.URL+"/big.ts", nil, "")
	if err == nil {
		t.Error("expected error for oversized segment, got nil")
	}
	if status != http.StatusBadGateway {
		t.Errorf("status: got %d want 502", status)
	}
	entries, _ := h.CacheStats()
	if entries != 0 {
		t.Errorf("oversized segment must not be cached, got %d entries", entries)
	}
}

// TestCachedFetchTooLargeNoCachingPath checks the same size guard on the
// non-caching (direct-fetch) path.
func TestCachedFetchTooLargeNoCachingPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.CopyN(w, zeroReader{}, maxSegmentBytes+1)
	}))
	defer srv.Close()

	h := New(Config{Client: srv.Client()}) // no cache
	ctx := t.Context()

	_, _, status, err := h.cachedFetch(ctx, srv.URL+"/big.ts", nil, "")
	if err == nil {
		t.Error("expected error for oversized segment (no-cache path), got nil")
	}
	if status != http.StatusBadGateway {
		t.Errorf("status: got %d want 502", status)
	}
}

// TestCachedFetchUpstreamError verifies that a fetch error is propagated.
func TestCachedFetchUpstreamError(t *testing.T) {
	// Use an address that should immediately refuse connection.
	h := New(Config{SegCacheTTL: time.Minute, Client: http.DefaultClient})
	ctx := t.Context()

	_, _, _, err := h.cachedFetch(ctx, "http://127.0.0.1:1/notexist.ts", nil, "")
	if err == nil {
		t.Error("expected error for unreachable upstream, got nil")
	}
}
