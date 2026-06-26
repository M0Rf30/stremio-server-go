package streamproxy

import (
	"container/list"
	"context"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// defaultCacheMaxBytes is the default total-byte cap for the segment cache (~256 MB).
const defaultCacheMaxBytes = 256 * 1024 * 1024

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

// cachedFetch fetches rawurl, using the segment cache when configured.
// Returns body, response headers, HTTP status, and any error.
func (h *Handler) cachedFetch(ctx context.Context, rawurl string, hdr http.Header, proxyURL string) ([]byte, http.Header, int, error) {
	if h.cache == nil || h.cfg.SegCacheTTL == 0 {
		// Caching disabled — fetch directly.
		resp, err := h.fetch(ctx, http.MethodGet, rawurl, hdr, nil, proxyURL)
		if err != nil {
			return nil, nil, 0, err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		return data, resp.Header, resp.StatusCode, err
	}

	// Cache hit.
	if entry := h.cache.getFull(rawurl); entry != nil {
		outHdr := make(http.Header)
		if entry.hdr != nil {
			for k, vs := range entry.hdr {
				outHdr[k] = vs
			}
		} else {
			outHdr.Set("Content-Length", strconv.Itoa(len(entry.val)))
		}
		return entry.val, outHdr, entry.status, nil
	}

	// Cache miss — fetch, store, return.
	resp, err := h.fetch(ctx, http.MethodGet, rawurl, hdr, nil, proxyURL)
	if err != nil {
		return nil, nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, resp.StatusCode, err
	}
	if resp.StatusCode == http.StatusOK {
		h.cache.putFull(rawurl, data, resp.Header.Clone(), resp.StatusCode)
	}
	return data, resp.Header, resp.StatusCode, nil
}

// prefetch asynchronously warms the cache for up to cfg.Prebuffer URLs.
// Errors are silently ignored. No-op when Prebuffer <= 0 or SegCacheTTL == 0.
func (h *Handler) prefetch(ctx context.Context, urls []string, hdr http.Header, proxyURL string) {
	if h.cfg.Prebuffer <= 0 || h.cache == nil {
		return
	}
	limit := h.cfg.Prebuffer
	if limit > len(urls) {
		limit = len(urls)
	}
	for _, u := range urls[:limit] {
		u := u
		go func() {
			h.cachedFetch(ctx, u, hdr, proxyURL) //nolint:errcheck
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
