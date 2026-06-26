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
		AppPath:    t.TempDir(),
		CacheRoot:  t.TempDir(),
		ListenPort: 0, // OS-assigned
		Version:    "test",
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
