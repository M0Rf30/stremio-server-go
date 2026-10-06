// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package engine

import (
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/types"
)

// sparseFile creates a file whose apparent size is size but which only has a few
// KiB written at the front, the shape the torrent file storage produces for a
// partially downloaded torrent. It skips the test when the filesystem does not
// create holes (the allocated size would then equal the apparent size).
func sparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if _, err := f.WriteAt(make([]byte, 4096), 0); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if allocatedSize(info) >= info.Size() {
		t.Skip("filesystem does not support sparse files (or allocated size is unavailable on this platform)")
	}
}

func TestAllocatedSizeIgnoresHoles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sparse.part")
	const apparent = 64 << 20
	sparseFile(t, path, apparent)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := allocatedSize(info); got <= 0 || got >= apparent/2 {
		t.Errorf("allocatedSize = %d for a %d-byte file with 4 KiB written; want a small non-zero value", got, apparent)
	}
	// A dense file reports (at most) its own length.
	dense := filepath.Join(t.TempDir(), "dense")
	if err := os.WriteFile(dense, make([]byte, 100_000), 0o644); err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(dense)
	if err != nil {
		t.Fatal(err)
	}
	if got := allocatedSize(di); got > di.Size() || got < di.Size()/2 {
		t.Errorf("allocatedSize(dense) = %d; want ≈ %d and never more", got, di.Size())
	}
}

// TestEvictCountsAllocatedBytes guards the cache-budget accounting: a torrent
// whose storage files are preallocated sparse must be charged for the blocks it
// has actually downloaded, not for its full length. Charging the apparent size
// made a single big torrent (e.g. a 2 GiB movie of which a few MiB had been
// fetched) fill the cache budget and get every other idle torrent purged.
func TestEvictCountsAllocatedBytes(t *testing.T) {
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

	const ihOld = "08ada5a7a6183aae1e09d831df6748d566095a10"
	const ihMRU = "0a8735c7ea18c99a1a948ec707d9bf3e544fdd2b"
	for _, ih := range []string{ihOld, ihMRU} {
		if _, err := m.EnsureEngine(ih, types.AddOptions{}); err != nil {
			t.Fatalf("EnsureEngine %s: %v", ih, err)
		}
		if err := os.MkdirAll(filepath.Join(cfg.CacheRoot, ih), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// ihOld: 256 MiB apparent but only 4 KiB on disk, way under the 1 MiB budget
	// once measured by blocks.
	sparseFile(t, filepath.Join(cfg.CacheRoot, ihOld, "movie.mkv.part"), 256<<20)

	stale := time.Now().Add(-10 * time.Minute)
	m.engines[ihOld].mu.Lock()
	m.engines[ihOld].lastAccess = stale
	m.engines[ihOld].mu.Unlock()
	m.engines[ihMRU].mu.Lock()
	m.engines[ihMRU].lastAccess = time.Now()
	m.engines[ihMRU].mu.Unlock()

	const budget = 1 << 20
	m.evict(budget)
	if _, ok := m.engines[ihOld]; !ok {
		t.Fatal("evict purged a torrent using ~4 KiB of real disk because its sparse files are 256 MiB long")
	}

	// Real data over the budget still evicts: the stale torrent now holds 2 MiB
	// of actual (non-hole) bytes.
	// Incompressible content: a transparently-compressing filesystem would
	// otherwise report fewer blocks than bytes written.
	filled := make([]byte, 2<<20)
	_, _ = rand.New(rand.NewSource(1)).Read(filled)
	if err := os.WriteFile(filepath.Join(cfg.CacheRoot, ihOld, "filled.bin"), filled, 0o644); err != nil {
		t.Fatal(err)
	}
	m.evict(budget)
	if _, ok := m.engines[ihOld]; ok {
		t.Fatal("evict kept a stale torrent whose allocated bytes exceed the budget")
	}
	if _, ok := m.engines[ihMRU]; !ok {
		t.Fatal("MRU engine should be preserved")
	}
}
