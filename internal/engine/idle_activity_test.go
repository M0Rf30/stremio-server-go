// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"testing"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/types"
)

// newIdleTestManager returns a manager with one reader-less engine whose
// lastAccess has been pushed 10 minutes into the past.
func newIdleTestManager(t *testing.T, ih string) (*manager, *engine) {
	t.Helper()
	em, err := New(newJanitorTestCfg(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = em.Close() })
	m := em.(*manager)
	if _, err := m.EnsureEngine(ih, types.AddOptions{}); err != nil {
		t.Fatalf("EnsureEngine: %v", err)
	}
	e := m.engines[ih]
	e.mu.Lock()
	e.lastAccess = time.Now().Add(-10 * time.Minute)
	e.mu.Unlock()
	return m, e
}

// TestGlobalStatsPollingDoesNotKeepTorrentAlive is the regression test for
// issue #41 ("Idle torrent not evicted"): the global /stats.json (AllStats)
// used to bump every torrent's lastAccess, so any dashboard or monitor polling
// it reset the idle clock on every call and no torrent was ever reclaimed by
// STREMIO_TORRENT_IDLE_TIMEOUT.
func TestGlobalStatsPollingDoesNotKeepTorrentAlive(t *testing.T) {
	const ih = "08ada5a7a6183aae1e09d831df6748d566095a10"
	m, e := newIdleTestManager(t, ih)

	m.AllStats()
	m.AllStats()

	e.mu.Lock()
	idleFor := time.Since(e.lastAccess)
	e.mu.Unlock()
	if idleFor < 9*time.Minute {
		t.Fatalf("AllStats polling reset the idle clock (idle for only %s, want ~10m)", idleFor)
	}

	m.evictIdle(5 * time.Minute)
	if _, ok := m.engines[ih]; ok {
		t.Fatal("idle torrent was not evicted although only AllStats() polled it")
	}
}

// TestPerTorrentStatsKeepsTorrentAlive: the stremio-core player polls
// /{ih}[/{idx}]/stats.json only while that torrent is open, so a per-torrent
// Stats call is real use and must keep a paused stream resident for resume.
func TestPerTorrentStatsKeepsTorrentAlive(t *testing.T) {
	for _, idx := range []int{-1, 0} {
		const ih = "08ada5a7a6183aae1e09d831df6748d566095a10"
		m, e := newIdleTestManager(t, ih)

		e.Stats(idx)

		m.evictIdle(5 * time.Minute)
		if _, ok := m.engines[ih]; !ok {
			t.Fatalf("Stats(%d): torrent polled by its player was evicted as idle", idx)
		}
	}
}

// TestEvictIdleIndependentOfSeedRatio guards the issue's other half: pausing
// upload at the seed ratio (enforceSeedRatio) must neither refresh the idle
// clock nor protect the torrent from the idle pass.
func TestEvictIdleIndependentOfSeedRatio(t *testing.T) {
	const ih = "0a8735c7ea18c99a1a948ec707d9bf3e544fdd2b"
	m, e := newIdleTestManager(t, ih)

	m.enforceSeedRatio(0.5)

	e.mu.Lock()
	idleFor := time.Since(e.lastAccess)
	e.mu.Unlock()
	if idleFor < 9*time.Minute {
		t.Fatalf("enforceSeedRatio reset the idle clock (idle for only %s)", idleFor)
	}
	m.evictIdle(5 * time.Minute)
	if _, ok := m.engines[ih]; ok {
		t.Fatal("idle torrent survived the idle pass after a seed-ratio pass")
	}
}

// TestReaderCloseRestartsIdleClock: the idle clock must restart when a reader
// is released. Otherwise a long playback with no stats poller (lastAccess =
// time NewReader ran) would look idle for hours the instant the user pressed
// stop and be dropped on the very next janitor tick.
//
// Two readers share the file so the last-reader demotion path (which needs a
// torrent with metadata) is not taken; the lastAccess bump is unconditional.
func TestReaderCloseRestartsIdleClock(t *testing.T) {
	const ih = "0b8735c7ea18c99a1a948ec707d9bf3e544fdd2c"
	m, e := newIdleTestManager(t, ih)

	ctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	e.openReaders = 2
	e.reading = map[int]int{0: 2}
	e.mu.Unlock()
	pr := &pinnedReader{tr: newFakeTorrentReader(), e: e, idx: 0, ctx: ctx, cancel: cancel}

	if err := pr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	e.mu.Lock()
	idleFor := time.Since(e.lastAccess)
	readers := e.openReaders
	e.mu.Unlock()
	if readers != 1 {
		t.Fatalf("openReaders = %d, want 1", readers)
	}
	if idleFor > time.Minute {
		t.Fatalf("reader Close did not restart the idle clock (idle for %s)", idleFor)
	}

	// Release the other reader by hand: now reader-less and freshly active, so
	// the idle pass must leave it for the full window.
	e.mu.Lock()
	e.openReaders = 0
	e.mu.Unlock()
	m.evictIdle(5 * time.Minute)
	if _, ok := m.engines[ih]; !ok {
		t.Fatal("torrent evicted right after its reader closed; it should survive the idle window")
	}
}
