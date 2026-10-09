// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package streamproxy

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// heapPeak samples runtime.MemStats.HeapAlloc in the background and records
// the highest value seen above the baseline taken at start. HeapAlloc counts
// live objects plus garbage not yet swept, which is the footprint the Go
// runtime actually holds on the process's behalf.
type heapPeak struct {
	base uint64
	peak atomic.Uint64
	stop chan struct{}
	done chan struct{}
}

func startHeapPeak() *heapPeak {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	p := &heapPeak{base: ms.HeapAlloc, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > p.peak.Load() {
				p.peak.Store(m.HeapAlloc)
			}
			select {
			case <-p.stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	return p
}

// finish stops sampling and returns the peak growth over the baseline in bytes.
func (p *heapPeak) finish() uint64 {
	close(p.stop)
	<-p.done
	if pk := p.peak.Load(); pk > p.base {
		return pk - p.base
	}
	return 0
}

// lowGC makes the collector run eagerly so HeapAlloc tracks live memory
// rather than garbage awaiting the next cycle; the returned func restores it.
func lowGC() func() {
	old := debug.SetGCPercent(5)
	return func() { debug.SetGCPercent(old) }
}

// segGate is an upstream serving n distinct segments of size bytes each with
// a known Content-Length. Every handler sends the headers plus a first chunk,
// then parks until all n requests have arrived (or a timeout), so the proxy
// sees all n fetches in flight at once before any body completes.
type segGate struct {
	*httptest.Server
	arrived atomic.Int32
}

func newSegGate(n, size int, body []byte) *segGate {
	g := &segGate{}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(size))
		w.WriteHeader(http.StatusOK)
		const first = 32 * 1024
		_, _ = w.Write(body[:first])
		w.(http.Flusher).Flush()
		g.arrived.Add(1)
		for deadline := time.Now().Add(5 * time.Second); int(g.arrived.Load()) < n && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
		_, _ = w.Write(body[first:size])
	}))
	return g
}

// benchSegments drives n concurrent segment requests (one goroutine each,
// distinct URLs) via do and reports the heap peak per iteration.
func benchSegments(b *testing.B, n int, do func(i int, base string)) {
	b.Helper()
	const size = 8 << 20
	body := make([]byte, size)
	defer lowGC()()
	b.ReportAllocs()
	var worst uint64
	for b.Loop() {
		g := newSegGate(n, size, body)
		hp := startHeapPeak()
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				do(i, g.URL)
			}()
		}
		wg.Wait()
		if pk := hp.finish(); pk > worst {
			worst = pk
		}
		g.Close()
	}
	b.ReportMetric(float64(worst)/(1<<20), "peak-MiB")
}

// newBenchHandler returns a handler whose segment cache is small enough that
// retained entries do not mask the in-flight footprint being measured.
func newBenchHandler(b *testing.B, ttl time.Duration, client *http.Client) *Handler {
	b.Helper()
	h := New(Config{SegCacheTTL: ttl, Client: client, PublicURL: "https://ext.example"})
	if h.cache != nil {
		h.cache.maxBytes = 16 << 20
	}
	b.Cleanup(h.Close)
	return h
}

// BenchmarkSegmentFetchConcurrent: 32 concurrent 8 MiB prefetch-style
// fetches (256 MiB of demand) through cachedFetch.
func BenchmarkSegmentFetchConcurrent(b *testing.B) {
	const n = 32
	h := newBenchHandler(b, time.Minute, &http.Client{})
	benchSegments(b, n, func(i int, base string) {
		_, _, _, _ = h.cachedFetch(b.Context(), fmt.Sprintf("%s/seg%d-%d.ts", base, i, time.Now().UnixNano()), nil, "")
	})
}

// BenchmarkServeStreamCachedConcurrent: 32 concurrent 8 MiB non-range GETs
// (256 MiB of demand) through serveStream with the segment cache on.
func BenchmarkServeStreamCachedConcurrent(b *testing.B) {
	const n = 32
	h := newBenchHandler(b, time.Minute, &http.Client{})
	benchSegments(b, n, func(i int, base string) {
		u := fmt.Sprintf("%s/seg%d-%d.ts", base, i, time.Now().UnixNano())
		req := httptest.NewRequest("GET", "/proxy/stream?d="+url.QueryEscape(u), nil)
		h.serveStream(&countingWriter{hdr: make(http.Header)}, req)
	})
}

// BenchmarkServeStreamDecryptConcurrent: 16 concurrent 8 MiB AES-128 HLS
// segments (128 MiB of ciphertext) decrypted by serveStream.
func BenchmarkServeStreamDecryptConcurrent(b *testing.B) {
	const n = 16
	const size = 8 << 20
	key := []byte("0123456789abcdef")
	iv := []byte("fedcba9876543210")
	plain := make([]byte, size)
	for i := range plain {
		plain[i] = byte(i)
	}
	plain = drmPKCS7Pad(plain[:size-1], aes.BlockSize)[:size]
	blk, err := aes.NewCipher(key)
	if err != nil {
		b.Fatal(err)
	}
	ct := make([]byte, size)
	cipher.NewCBCEncrypter(blk, iv).CryptBlocks(ct, plain)

	h := newBenchHandler(b, 0, &http.Client{})
	defer lowGC()()
	b.ReportAllocs()
	var worst uint64
	for b.Loop() {
		g := newSegGate(n, size, ct)
		hp := startHeapPeak()
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				u := fmt.Sprintf("%s/seg%d.ts", g.URL, i)
				q := "/proxy/stream?d=" + url.QueryEscape(u) + "&method=AES-128&key=" + hex.EncodeToString(key) + "&iv=" + hex.EncodeToString(iv)
				h.serveStream(&countingWriter{hdr: make(http.Header)}, httptest.NewRequest("GET", q, nil))
			}()
		}
		wg.Wait()
		if pk := hp.finish(); pk > worst {
			worst = pk
		}
		g.Close()
	}
	b.ReportMetric(float64(worst)/(1<<20), "peak-MiB")
}
