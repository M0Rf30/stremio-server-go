package streamproxy

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// defaultCacheMaxBytes is the default total-byte cap for the segment cache (~256 MB).
const defaultCacheMaxBytes = 256 * 1024 * 1024

// maxSegmentBytes is the per-segment read ceiling inside cachedFetch.
// Upstream responses exceeding this value are refused rather than buffered
// unboundedly; 50 MiB is generous for any real HLS/DASH segment.
const maxSegmentBytes int64 = 50 * 1024 * 1024

// cacheEntry holds a cached segment with its metadata.
type cacheEntry struct {
	key       string
	val       []byte
	hdr       http.Header
	status    int
	expiresAt time.Time
	size      int64 // len(val), tracked for byte-budget eviction
}

// segCache is an in-memory LRU segment cache bounded by entry count, total bytes, and TTL.
type segCache struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	maxBytes   int64
	totalBytes int64
	items      map[string]*list.Element
	lru        *list.List
}

// newSegCache creates a segCache with the given TTL and entry cap.
// The total-byte budget defaults to defaultCacheMaxBytes.
func newSegCache(ttl time.Duration, maxEntries int) *segCache {
	return &segCache{
		ttl:        ttl,
		maxEntries: maxEntries,
		maxBytes:   defaultCacheMaxBytes,
		items:      make(map[string]*list.Element),
		lru:        list.New(),
	}
}

// putFull stores a full response entry (body + headers + status).
// It evicts least-recently-used entries to satisfy both the entry cap and the byte budget.
func (c *segCache) putFull(key string, val []byte, hdr http.Header, status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	newSize := int64(len(val))
	if el, ok := c.items[key]; ok {
		c.lru.MoveToFront(el)
		e := el.Value.(*cacheEntry)
		c.totalBytes -= e.size
		c.totalBytes += newSize
		e.val = val
		e.hdr = hdr
		e.status = status
		e.size = newSize
		e.expiresAt = time.Now().Add(c.ttl)
		return
	}
	// Evict until both constraints are satisfied.
	for c.lru.Len() >= c.maxEntries || c.totalBytes+newSize > c.maxBytes {
		back := c.lru.Back()
		if back == nil {
			break
		}
		evicted := back.Value.(*cacheEntry)
		c.totalBytes -= evicted.size
		delete(c.items, evicted.key)
		c.lru.Remove(back)
	}
	entry := &cacheEntry{
		key:       key,
		val:       val,
		hdr:       hdr,
		status:    status,
		expiresAt: time.Now().Add(c.ttl),
		size:      newSize,
	}
	c.totalBytes += newSize
	c.items[key] = c.lru.PushFront(entry)
}

// getFull returns the full cache entry, or nil when absent or expired.
func (c *segCache) getFull(key string) *cacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil
	}
	entry := el.Value.(*cacheEntry)
	if time.Now().After(entry.expiresAt) {
		c.totalBytes -= entry.size
		c.lru.Remove(el)
		delete(c.items, key)
		return nil
	}
	c.lru.MoveToFront(el)
	return entry
}

// sha256Pool recycles hash.Hash instances to avoid per-call allocations on
// the segment-request hot path.  Reset is called before each use.
var sha256Pool = sync.Pool{New: func() any { return sha256.New() }}

// cacheKey derives a cache lookup key from the raw URL plus any credential
// headers present in hdr. Authorization and Cookie are folded in so that
// two requests for the same URL but different credentials never collide.
// The result is a hex-encoded SHA-256 digest, safe to use as a map key.
func cacheKey(rawurl string, hdr http.Header) string {
	// F4: pool the hasher; avoids sha256.New() + fmt.Fprintf reflection on every request.
	h := sha256Pool.Get().(hash.Hash)
	h.Reset()
	_, _ = h.Write([]byte(rawurl))
	// Include auth-relevant request headers to prevent cross-credential cache
	// poisoning. Use NUL/colon separators so a crafted URL cannot produce the
	// same hash as a URL-plus-header combination.
	for _, name := range []string{"Authorization", "Cookie"} {
		vs := hdr[http.CanonicalHeaderKey(name)]
		if len(vs) > 0 {
			_, _ = h.Write([]byte{0})
			_, _ = h.Write([]byte(name))
			_, _ = h.Write([]byte{':'})
			for _, v := range vs {
				_, _ = h.Write([]byte(v))
				_, _ = h.Write([]byte{1})
			}
		}
	}
	var buf [32]byte
	h.Sum(buf[:0])
	sha256Pool.Put(h)
	return hex.EncodeToString(buf[:])
}

// cachedFetch fetches rawurl, using the segment cache when configured.
// Returns body, response headers, HTTP status, and any error.
func (h *Handler) cachedFetch(ctx context.Context, rawurl string, hdr http.Header, proxyURL string) ([]byte, http.Header, int, error) {
	if h.cache == nil || h.cfg.SegCacheTTL == 0 {
		// Caching disabled — fetch directly, but cap the read to prevent OOM.
		resp, err := h.fetch(ctx, http.MethodGet, rawurl, hdr, nil, proxyURL)
		if err != nil {
			return nil, nil, 0, err
		}
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxSegmentBytes+1))
		if err != nil {
			return nil, nil, resp.StatusCode, err
		}
		if int64(len(data)) > maxSegmentBytes {
			return nil, nil, http.StatusBadGateway,
				fmt.Errorf("upstream segment too large (> %d bytes)", maxSegmentBytes)
		}
		return data, resp.Header, resp.StatusCode, nil
	}

	// Cache hit.
	if entry := h.cache.getFull(cacheKey(rawurl, hdr)); entry != nil {
		if entry.hdr != nil {
			// F6: return stored clone directly; callers (copyAllowedHeaders,
			// applyRespHeaders) only read the map — no defensive copy needed.
			return entry.val, entry.hdr, entry.status, nil
		}
		// hdr was nil at store time — synthesise a minimal Content-Length header.
		outHdr := make(http.Header)
		outHdr.Set("Content-Length", strconv.Itoa(len(entry.val)))
		return entry.val, outHdr, entry.status, nil
	}

	// Cache miss — fetch, store, return.
	resp, err := h.fetch(ctx, http.MethodGet, rawurl, hdr, nil, proxyURL)
	if err != nil {
		return nil, nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	// Bound the read: if the segment exceeds maxSegmentBytes we do not cache it
	// (skip caching) and return an error rather than buffering unboundedly.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSegmentBytes+1))
	if err != nil {
		return nil, nil, resp.StatusCode, err
	}
	if int64(len(data)) > maxSegmentBytes {
		return nil, nil, http.StatusBadGateway,
			fmt.Errorf("upstream segment too large to cache (> %d bytes)", maxSegmentBytes)
	}
	if resp.StatusCode == http.StatusOK {
		h.cache.putFull(cacheKey(rawurl, hdr), data, resp.Header.Clone(), resp.StatusCode)
	}
	return data, resp.Header, resp.StatusCode, nil
}

// prefetch asynchronously warms the cache for up to cfg.Prebuffer URLs.
// Each goroutine runs under a fresh context bounded by prefetchTimeout so it
// cannot outlive a slow upstream indefinitely. Concurrent goroutines are
// capped by h.prefetchSem; if all slots are occupied the remaining URLs are
// skipped rather than blocked. Errors are silently ignored.
// No-op when Prebuffer <= 0 or SegCacheTTL == 0.
func (h *Handler) prefetch(ctx context.Context, urls []string, hdr http.Header, proxyURL string) {
	if h.cfg.Prebuffer <= 0 || h.cache == nil {
		return
	}
	limit := h.cfg.Prebuffer
	if limit > len(urls) {
		limit = len(urls)
	}
	for _, u := range urls[:limit] {
		// Non-blocking semaphore acquire — skip remaining URLs when all slots busy.
		select {
		case h.prefetchSem <- struct{}{}:
		default:
			return
		}
		go func() {
			defer func() { <-h.prefetchSem }()
			// Detach from the request's cancellation (prefetch must outlive the
			// originating response) while keeping its values and an explicit
			// wall-clock bound.
			pCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), prefetchTimeout)
			defer cancel()
			_, _, _, _ = h.cachedFetch(pCtx, u, hdr, proxyURL)
		}()
	}
}

// CacheStats returns the number of entries and total bytes held by the segment
// cache.  Returns (0, 0) when the cache is not configured.
func (h *Handler) CacheStats() (entries int, bytes int64) {
	if h.cache == nil {
		return 0, 0
	}
	h.cache.mu.Lock()
	defer h.cache.mu.Unlock()
	return len(h.cache.items), h.cache.totalBytes
}
