// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package engine

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/types"
)

const (
	scanIhOld = "08ada5a7a6183aae1e09d831df6748d566095a10" // stale, evictable
	scanIhMRU = "0a8735c7ea18c99a1a948ec707d9bf3e544fdd2b" // most recently used, always kept
	scanIhNew = "2b8735c7ea18c99a1a948ec707d9bf3e544fdd2b" // added mid-test
	scanLimit = 1 << 20                                    // cache budget used by the tests
)

// writeIncompressible writes n random bytes to path (random so a transparently
// compressing filesystem cannot report fewer blocks than bytes).
func writeIncompressible(t *testing.T, path string, n int) {
	t.Helper()
	data := make([]byte, n)
	_, _ = rand.NewChaCha8([32]byte{byte(n)}).Read(data)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// scanFixture returns a manager holding a stale engine (4 KiB on disk) and a
// fresh one, both with a cache directory, so a tick under scanLimit measures
// the cache and finds it within budget.
func scanFixture(t *testing.T, cfg types.Config) *manager {
	t.Helper()
	em, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = em.Close() })
	m := em.(*manager)
	for _, ih := range []string{scanIhOld, scanIhMRU} {
		if _, err := m.EnsureEngine(ih, types.AddOptions{}); err != nil {
			t.Fatalf("EnsureEngine %s: %v", ih, err)
		}
		if err := os.MkdirAll(filepath.Join(cfg.CacheRoot, ih), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeIncompressible(t, filepath.Join(cfg.CacheRoot, scanIhOld, "small.bin"), 4096)

	m.engines[scanIhOld].mu.Lock()
	m.engines[scanIhOld].lastAccess = time.Now().Add(-10 * time.Minute)
	m.engines[scanIhOld].mu.Unlock()
	m.engines[scanIhMRU].mu.Lock()
	m.engines[scanIhMRU].lastAccess = time.Now()
	m.engines[scanIhMRU].mu.Unlock()
	return m
}

func (m *manager) hasEngine(ih string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.engines[ih]
	return ok
}

// settle runs the two measuring ticks after which the janitor trusts an
// under-budget reading: the first only records the counters, the second finds
// them unchanged and memoises the walk.
func settle(t *testing.T, m *manager) {
	t.Helper()
	m.evictTick(scanLimit)
	m.evictTick(scanLimit)
	if m.scan.dl == nil {
		t.Fatal("two under-budget walks with still counters left no memo")
	}
}

// TestEvictTickSkipsWalkWhenCacheQuiet: once ticks have found the cache within
// budget with still counters, later ticks skip the filesystem walk while the
// same engines are loaded and none has received data, until the reading ages
// out. The skip is observed through a file written behind the janitor's back:
// only a walk could see it.
func TestEvictTickSkipsWalkWhenCacheQuiet(t *testing.T) {
	cfg := newJanitorTestCfg(t)
	m := scanFixture(t, cfg)

	settle(t, m)
	writeIncompressible(t, filepath.Join(cfg.CacheRoot, scanIhOld, "big.bin"), 2<<20)

	m.evictTick(scanLimit)
	if !m.hasEngine(scanIhOld) {
		t.Fatal("quiet tick walked the cache (evicted on data it should not have looked at)")
	}

	m.scan.mu.Lock()
	m.scan.at = time.Now().Add(-diskScanMaxAge - time.Second)
	m.scan.mu.Unlock()
	m.evictTick(scanLimit)
	if m.hasEngine(scanIhOld) {
		t.Fatal("aged memo was trusted: the backstop walk did not evict the over-budget engine")
	}
	if !m.hasEngine(scanIhMRU) {
		t.Fatal("MRU engine should be preserved")
	}
}

// TestEvictTickDoesNotTrustFirstMeasurement: the library counts received bytes
// before the chunk is written, so a counter that was already bumped when first
// sampled says nothing about bytes still in flight. A single within-budget walk
// must not start skipping ticks; a write landing right after it (and with no
// further counter movement) has to be seen by the very next tick.
func TestEvictTickDoesNotTrustFirstMeasurement(t *testing.T) {
	cfg := newJanitorTestCfg(t)
	m := scanFixture(t, cfg)

	m.evictTick(scanLimit)
	writeIncompressible(t, filepath.Join(cfg.CacheRoot, scanIhOld, "big.bin"), 2<<20) // counted earlier, landed late

	m.evictTick(scanLimit)
	if m.hasEngine(scanIhOld) {
		t.Fatal("tick right after the first measurement skipped the walk and missed the late write")
	}
}

// TestEvictTickDoesNotMemoiseWhileCountersMove: a counter that differs from the
// one sampled before the previous walk means data arrived in between, so the
// walk is not memoised even though it was within budget.
func TestEvictTickDoesNotMemoiseWhileCountersMove(t *testing.T) {
	cfg := newJanitorTestCfg(t)
	m := scanFixture(t, cfg)

	m.evictTick(scanLimit)
	m.scan.mu.Lock()
	m.scan.prev[m.engines[scanIhMRU]]-- // the counter moved since the previous sample
	m.scan.mu.Unlock()
	m.evictTick(scanLimit)
	if m.scan.dl != nil {
		t.Fatal("a walk preceded by counter movement was memoised")
	}
	writeIncompressible(t, filepath.Join(cfg.CacheRoot, scanIhOld, "big.bin"), 2<<20)
	m.evictTick(scanLimit)
	if m.hasEngine(scanIhOld) {
		t.Fatal("tick after counter movement skipped the walk")
	}
}

// TestEvictTickWalksWhenBudgetShrinks: a memo taken under a larger budget must
// not hide an over-budget cache once the budget drops below the measured total.
func TestEvictTickWalksWhenBudgetShrinks(t *testing.T) {
	m := scanFixture(t, newJanitorTestCfg(t))

	m.evictTick(scanLimit)
	if !m.hasEngine(scanIhOld) {
		t.Fatal("engine evicted while under budget")
	}
	m.evictTick(1) // far below the 4 KiB on disk
	if m.hasEngine(scanIhOld) {
		t.Fatal("shrunken budget was ignored: the stale engine was not evicted")
	}
}

// TestEvictTickWalksWhenEngineSetChanges: a newly loaded engine may already own
// cached data, so the memo no longer covers the cache and the next tick walks.
func TestEvictTickWalksWhenEngineSetChanges(t *testing.T) {
	cfg := newJanitorTestCfg(t)
	m := scanFixture(t, cfg)

	settle(t, m)
	writeIncompressible(t, filepath.Join(cfg.CacheRoot, scanIhOld, "big.bin"), 2<<20)
	if _, err := m.EnsureEngine(scanIhNew, types.AddOptions{}); err != nil {
		t.Fatalf("EnsureEngine: %v", err)
	}
	m.evictTick(scanLimit)
	if m.hasEngine(scanIhOld) {
		t.Fatal("tick after an engine was added did not walk the cache")
	}
}

// TestEvictTickWalksWhenDataReceived: a changed peer-data counter means pieces
// may have been written, so the memo is not trusted.
func TestEvictTickWalksWhenDataReceived(t *testing.T) {
	cfg := newJanitorTestCfg(t)
	m := scanFixture(t, cfg)

	settle(t, m)
	writeIncompressible(t, filepath.Join(cfg.CacheRoot, scanIhOld, "big.bin"), 2<<20)
	m.scan.mu.Lock()
	m.scan.dl[m.engines[scanIhMRU]]-- // the counter no longer matches the memo
	m.scan.mu.Unlock()
	m.evictTick(scanLimit)
	if m.hasEngine(scanIhOld) {
		t.Fatal("tick after data was received did not walk the cache")
	}
}

// TestEvictInRAMModeNeverWalks: with the in-RAM cache nothing is written to
// disk, so size-based eviction has nothing to measure and must not stat the
// cache directory (even one that holds files).
func TestEvictInRAMModeNeverWalks(t *testing.T) {
	cfg := newJanitorTestCfg(t)
	cfg.MemoryCacheSize = 1 << 20
	m := scanFixture(t, cfg)
	writeIncompressible(t, filepath.Join(cfg.CacheRoot, scanIhOld, "big.bin"), 2<<20)

	m.evict(1)
	m.evictTick(1)
	if !m.hasEngine(scanIhOld) {
		t.Fatal("in-RAM mode evicted by on-disk size")
	}
	if m.scan.dl != nil {
		t.Fatal("in-RAM mode measured the cache")
	}
}

// TestEvictIgnoresDirsOfUnloadedTorrents: cache directories that belong to no
// loaded engine are not walked and never count against the budget.
func TestEvictIgnoresDirsOfUnloadedTorrents(t *testing.T) {
	cfg := newJanitorTestCfg(t)
	m := scanFixture(t, cfg)
	orphan := filepath.Join(cfg.CacheRoot, scanIhNew)
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIncompressible(t, filepath.Join(orphan, "big.bin"), 2<<20)

	m.evict(scanLimit)
	if !m.hasEngine(scanIhOld) {
		t.Fatal("an unloaded torrent's directory was charged to the live engines")
	}
}

// benchCacheManager builds a manager (no torrent client) with live engines whose
// cache directories each hold files empty files, plus orphans directories of
// unloaded torrents holding the same.
func benchCacheManager(b *testing.B, live, orphans, files int) *manager {
	b.Helper()
	root := b.TempDir()
	m := &manager{
		cfg:     types.Config{CacheRoot: root},
		engines: make(map[string]*engine, live),
		purging: map[string]chan struct{}{},
	}
	for i := range live + orphans {
		ih := fmt.Sprintf("%040x", i+1)
		dir := filepath.Join(root, ih)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatal(err)
		}
		for f := range files {
			fh, err := os.Create(filepath.Join(dir, fmt.Sprintf("f%05d", f)))
			if err != nil {
				b.Fatal(err)
			}
			_ = fh.Close()
		}
		if i < live {
			m.engines[ih] = &engine{infoHash: ih, lastAccess: time.Now()}
		}
	}
	return m
}

// BenchmarkEvictWalk is the cost of one size-based janitor pass that does walk
// the cache: 20 loaded torrents of 250 files each, in a cache root that also
// holds 60 leftover torrent directories of 250 files.
func BenchmarkEvictWalk(b *testing.B) {
	m := benchCacheManager(b, 20, 60, 250)
	b.ReportAllocs()
	for b.Loop() {
		m.evict(1 << 40)
	}
}

// BenchmarkEvictTickQuiet is the same janitor tick once the cache is known to be
// within budget and untouched: no filesystem access.
func BenchmarkEvictTickQuiet(b *testing.B) {
	m := benchCacheManager(b, 20, 60, 250)
	m.evictTick(1 << 40) // the measuring walks: the reading is trusted after
	m.evictTick(1 << 40) // a second one finds the counters unchanged
	b.ReportAllocs()
	for b.Loop() {
		m.evictTick(1 << 40)
	}
}
