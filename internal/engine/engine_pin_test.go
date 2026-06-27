package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/types"
)

// TestEvictSkipsOpenReaders verifies the janitor never evicts a torrent that
// has an open reader (openReaders > 0), even when it is the stale LRU candidate,
// and that the same torrent becomes evictable once the reader is released. This
// guards the playback-reliability fix: a paused/idle stream must not be dropped
// out from under its HTTP reader.
func TestEvictSkipsOpenReaders(t *testing.T) {
	cfg := types.Config{
		AppPath:           t.TempDir(),
		CacheRoot:         t.TempDir(),
		ListenPort:        0, // OS-assigned
		Version:           "test",
		DisableTrackers:   true, // avoid upstream anacrolix tracker/udp -race flake in tests
		DisableWebtorrent: true, // avoid spawning pion goroutines in unit tests
	}
	em, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer em.Close()
	m := em.(*manager)

	const ihPinned = "08ada5a7a6183aae1e09d831df6748d566095a10" // stale LRU candidate
	const ihMRU = "0a8735c7ea18c99a1a948ec707d9bf3e544fdd2b"    // most-recently-used (always preserved)

	for _, ih := range []string{ihPinned, ihMRU} {
		if _, err := m.EnsureEngine(ih, types.AddOptions{}); err != nil {
			t.Fatalf("EnsureEngine %s: %v", ih, err)
		}
		// Give each engine on-disk cache so total exceeds the budget and the
		// janitor actually runs its eviction loop.
		dir := filepath.Join(cfg.CacheRoot, ih)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "data"), make([]byte, 1024), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ePinned := m.engines[ihPinned]
	eMRU := m.engines[ihMRU]

	// Make the pinned engine the oldest (eviction candidate) with an open reader;
	// the MRU engine is recent and is always preserved by the loop anyway.
	stale := time.Now().Add(-10 * time.Minute)
	ePinned.mu.Lock()
	ePinned.lastAccess = stale
	ePinned.openReaders = 1
	ePinned.mu.Unlock()
	eMRU.mu.Lock()
	eMRU.lastAccess = time.Now()
	eMRU.mu.Unlock()

	// Budget far below the 2 KiB on disk: eviction runs, but the pinned engine
	// must be skipped despite being the stale LRU candidate.
	m.evict(1)
	if _, ok := m.engines[ihPinned]; !ok {
		t.Fatal("engine with openReaders>0 was evicted")
	}

	// Release the reader: the stale engine is now an eligible candidate and must
	// be evicted on the next pass; the MRU engine stays.
	ePinned.mu.Lock()
	ePinned.openReaders = 0
	ePinned.mu.Unlock()
	m.evict(1)
	if _, ok := m.engines[ihPinned]; ok {
		t.Fatal("stale unpinned engine was not evicted")
	}
	if _, ok := m.engines[ihMRU]; !ok {
		t.Fatal("MRU engine should be preserved")
	}
}

// TestEvictIdle verifies the inactivity reclaim: a torrent with no open readers
// and a last-access older than the idle timeout is dropped, while a recently
// accessed one and a pinned (openReaders>0) one are preserved. It also checks
// that idle<=0 disables the pass entirely (matching the size-based evict's
// unlimited semantics).
func TestEvictIdle(t *testing.T) {
	cfg := types.Config{
		AppPath:           t.TempDir(),
		CacheRoot:         t.TempDir(),
		ListenPort:        0,
		Version:           "test",
		DisableTrackers:   true,
		DisableWebtorrent: true,
	}
	em, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer em.Close()
	m := em.(*manager)

	const ihIdle = "08ada5a7a6183aae1e09d831df6748d566095a10"   // stale, unpinned -> removed
	const ihRecent = "0a8735c7ea18c99a1a948ec707d9bf3e544fdd2b" // recently accessed -> kept
	const ihPinned = "0b8735c7ea18c99a1a948ec707d9bf3e544fdd2c" // stale but openReaders>0 -> kept

	for _, ih := range []string{ihIdle, ihRecent, ihPinned} {
		if _, err := m.EnsureEngine(ih, types.AddOptions{}); err != nil {
			t.Fatalf("EnsureEngine %s: %v", ih, err)
		}
	}

	const idle = 5 * time.Minute
	stale := time.Now().Add(-10 * time.Minute)

	m.engines[ihIdle].mu.Lock()
	m.engines[ihIdle].lastAccess = stale
	m.engines[ihIdle].mu.Unlock()

	m.engines[ihRecent].mu.Lock()
	m.engines[ihRecent].lastAccess = time.Now()
	m.engines[ihRecent].mu.Unlock()

	m.engines[ihPinned].mu.Lock()
	m.engines[ihPinned].lastAccess = stale
	m.engines[ihPinned].openReaders = 1
	m.engines[ihPinned].mu.Unlock()

	// idle<=0 must be a no-op even with a stale engine present.
	m.evictIdle(0)
	if _, ok := m.engines[ihIdle]; !ok {
		t.Fatal("evictIdle(0) removed an engine; should be disabled")
	}

	m.evictIdle(idle)
	if _, ok := m.engines[ihIdle]; ok {
		t.Fatal("stale unpinned engine was not removed by evictIdle")
	}
	if _, ok := m.engines[ihRecent]; !ok {
		t.Fatal("recently accessed engine was wrongly removed")
	}
	if _, ok := m.engines[ihPinned]; !ok {
		t.Fatal("pinned (openReaders>0) engine was wrongly removed")
	}
}

// TestReadaheadFor verifies the adaptive readahead window: ~2 s of throughput,
// clamped to the [8 MiB, 64 MiB] floor/ceiling.
func TestReadaheadFor(t *testing.T) {
	const mib = 1 << 20
	cases := []struct {
		speed float64
		want  int64
	}{
		{0, 8 * mib},          // no measurement -> floor
		{1 * mib, 8 * mib},    // 2 MiB scaled < floor
		{3 * mib, 8 * mib},    // 6 MiB scaled < floor
		{10 * mib, 20 * mib},  // within range
		{32 * mib, 64 * mib},  // 64 MiB scaled == ceiling
		{100 * mib, 64 * mib}, // 200 MiB scaled -> ceiling
	}
	for _, c := range cases {
		if got := readaheadFor(c.speed); got != c.want {
			t.Errorf("readaheadFor(%.0f) = %d, want %d", c.speed, got, c.want)
		}
	}
}
