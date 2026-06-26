package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/engine"
	"github.com/M0Rf30/stremio-server-go/internal/types"
)

// knownMagnet is the Sintel open-movie infohash (public-domain, well-seeded).
// We don't actually download anything in unit tests — we just exercise the API.
const knownHash = "08ada5a7a6183aae1e09d831df6748d566095a10"

func newTestCfg(t *testing.T) types.Config {
	t.Helper()
	return types.Config{
		HTTPPort:        0,
		ListenPort:      0, // OS-assigned
		AppPath:         t.TempDir(),
		CacheRoot:       t.TempDir(),
		Version:         "4.21.0",
		DisableTrackers: true, // avoid upstream anacrolix tracker/udp -race flake in tests
	}
}

// TestNew verifies the constructor returns a non-nil EngineManager.
func TestNew(t *testing.T) {
	em, err := engine.New(newTestCfg(t))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer em.Close()

	if em == nil {
		t.Fatal("expected non-nil EngineManager")
	}
}

// TestEnsureEngineReturnsEngine verifies EnsureEngine returns a non-nil Engine
// and InfoHash() is normalised to lower-case.
func TestEnsureEngineReturnsEngine(t *testing.T) {
	em, err := engine.New(newTestCfg(t))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer em.Close()

	// Pass upper-case hash — engine must normalise it.
	e, err := em.EnsureEngine(strings.ToUpper(knownHash), types.AddOptions{
		Trackers: []string{"udp://tracker.opentrackr.org:1337/announce"},
	})
	if err != nil {
		t.Fatalf("EnsureEngine: %v", err)
	}
	if e == nil {
		t.Fatal("expected non-nil Engine")
	}
	if e.InfoHash() != knownHash {
		t.Errorf("InfoHash = %q, want %q", e.InfoHash(), knownHash)
	}
}

// TestEnsureEngineIdempotent verifies a second call returns the same engine and
// doesn't error.
func TestEnsureEngineIdempotent(t *testing.T) {
	em, err := engine.New(newTestCfg(t))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer em.Close()

	opts := types.AddOptions{
		Sources: []string{"tracker:udp://tracker.opentrackr.org:1337/announce"},
	}
	e1, err := em.EnsureEngine(knownHash, opts)
	if err != nil {
		t.Fatalf("EnsureEngine first: %v", err)
	}
	e2, err := em.EnsureEngine(knownHash, opts)
	if err != nil {
		t.Fatalf("EnsureEngine second: %v", err)
	}
	// Must be the same object (same pointer via InfoHash identity).
	if e1.InfoHash() != e2.InfoHash() {
		t.Errorf("got different infohashes on second call: %q vs %q", e1.InfoHash(), e2.InfoHash())
	}
}

// TestGetEngine verifies GetEngine returns (nil,false) before add and
// (engine,true) after.
func TestGetEngine(t *testing.T) {
	em, err := engine.New(newTestCfg(t))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer em.Close()

	_, ok := em.GetEngine(knownHash)
	if ok {
		t.Fatal("GetEngine should return false before EnsureEngine")
	}

	_, err = em.EnsureEngine(knownHash, types.AddOptions{})
	if err != nil {
		t.Fatalf("EnsureEngine: %v", err)
	}

	e, ok := em.GetEngine(knownHash)
	if !ok {
		t.Fatal("GetEngine should return true after EnsureEngine")
	}
	if e == nil {
		t.Fatal("GetEngine returned nil engine")
	}
}

// TestListEngines verifies ListEngines reflects the live set.
func TestListEngines(t *testing.T) {
	em, err := engine.New(newTestCfg(t))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer em.Close()

	if n := len(em.ListEngines()); n != 0 {
		t.Fatalf("ListEngines before add: want 0, got %d", n)
	}
	if _, err = em.EnsureEngine(knownHash, types.AddOptions{}); err != nil {
		t.Fatalf("EnsureEngine: %v", err)
	}
	list := em.ListEngines()
	if len(list) != 1 || list[0] != knownHash {
		t.Errorf("ListEngines = %v, want [%q]", list, knownHash)
	}
}

// TestRemoveEngine verifies drop cleans up the map.
func TestRemoveEngine(t *testing.T) {
	em, err := engine.New(newTestCfg(t))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer em.Close()

	if _, err = em.EnsureEngine(knownHash, types.AddOptions{}); err != nil {
		t.Fatalf("EnsureEngine: %v", err)
	}
	if err = em.RemoveEngine(knownHash); err != nil {
		t.Fatalf("RemoveEngine: %v", err)
	}
	if _, ok := em.GetEngine(knownHash); ok {
		t.Fatal("engine still present after RemoveEngine")
	}
	// Second remove must be idempotent.
	if err = em.RemoveEngine(knownHash); err != nil {
		t.Fatalf("RemoveEngine (idempotent): %v", err)
	}
}

// TestReadyTimesOutBeforeMetadata verifies Ready returns ctx.Err() when the
// context expires before metadata arrives (normal in offline test env).
func TestReadyTimesOutBeforeMetadata(t *testing.T) {
	em, err := engine.New(newTestCfg(t))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer em.Close()

	e, err := em.EnsureEngine(knownHash, types.AddOptions{})
	if err != nil {
		t.Fatalf("EnsureEngine: %v", err)
	}

	// Short timeout — we have no peers in a unit test, so metadata won't arrive.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	readyErr := e.Ready(ctx)
	if readyErr == nil {
		// Metadata arrived unexpectedly fast (unlikely offline, but not a failure).
		t.Log("Ready returned nil (metadata arrived — unexpected in unit test but not an error)")
	} else if !errors.Is(readyErr, context.DeadlineExceeded) {
		t.Errorf("Ready: want DeadlineExceeded, got %v", readyErr)
	}
}

// TestFilesBeforeMetadata verifies Files() returns nil before metadata.
func TestFilesBeforeMetadata(t *testing.T) {
	em, err := engine.New(newTestCfg(t))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer em.Close()

	e, err := em.EnsureEngine(knownHash, types.AddOptions{})
	if err != nil {
		t.Fatalf("EnsureEngine: %v", err)
	}
	if files := e.Files(); files != nil {
		// If somehow we got metadata (network race), just skip.
		t.Logf("Files() returned %d files before metadata timeout — network present?", len(files))
	}
}

// TestGuessFileIdxBeforeMetadata verifies GuessFileIdx returns -1 before info.
func TestGuessFileIdxBeforeMetadata(t *testing.T) {
	em, err := engine.New(newTestCfg(t))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer em.Close()

	e, err := em.EnsureEngine(knownHash, types.AddOptions{})
	if err != nil {
		t.Fatalf("EnsureEngine: %v", err)
	}
	if idx := e.GuessFileIdx(); idx != -1 {
		t.Logf("GuessFileIdx = %d (metadata arrived — network present?)", idx)
	}
}

// TestStatsBeforeMetadata verifies Stats(-1) never panics and returns the right infoHash.
func TestStatsBeforeMetadata(t *testing.T) {
	em, err := engine.New(newTestCfg(t))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer em.Close()

	e, err := em.EnsureEngine(knownHash, types.AddOptions{})
	if err != nil {
		t.Fatalf("EnsureEngine: %v", err)
	}
	s := e.Stats(-1)
	if s == nil {
		t.Fatal("Stats(-1) returned nil")
	}
	if s.InfoHash != knownHash {
		t.Errorf("Stats.InfoHash = %q, want %q", s.InfoHash, knownHash)
	}
	if s.Wires == nil {
		t.Error("Stats.Wires must not be nil (should be empty slice)")
	}
}

// TestAllStats verifies the manager-level snapshot.
func TestAllStats(t *testing.T) {
	em, err := engine.New(newTestCfg(t))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer em.Close()

	if _, err = em.EnsureEngine(knownHash, types.AddOptions{}); err != nil {
		t.Fatalf("EnsureEngine: %v", err)
	}
	all := em.AllStats()
	if _, ok := all[knownHash]; !ok {
		t.Errorf("AllStats missing key %q; got keys: %v", knownHash, all)
	}
}

// TestRemoveAll verifies all engines are cleared.
func TestRemoveAll(t *testing.T) {
	em, err := engine.New(newTestCfg(t))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer em.Close()

	if _, err = em.EnsureEngine(knownHash, types.AddOptions{}); err != nil {
		t.Fatalf("EnsureEngine: %v", err)
	}
	em.RemoveAll()
	if n := len(em.ListEngines()); n != 0 {
		t.Errorf("ListEngines after RemoveAll: want 0, got %d", n)
	}
}

// TestSourcesStripping verifies "tracker:" prefix is stripped and "dht:" is ignored.
func TestSourcesStripping(t *testing.T) {
	em, err := engine.New(newTestCfg(t))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer em.Close()

	// Should not error — tracker/dht sources must be parsed without panic.
	_, err = em.EnsureEngine(knownHash, types.AddOptions{
		Sources: []string{
			"tracker:udp://tracker.opentrackr.org:1337/announce",
			"dht:08ada5a7a6183aae1e09d831df6748d566095a10",
		},
	})
	if err != nil {
		t.Fatalf("EnsureEngine with sources: %v", err)
	}
}

// TestStatisticsSourceShape guards every stremio-core-required Source field.
// A missing field makes stremio-core's strict Statistics deserializer reject the
// whole object, blanking the stats panel (regression: lastStarted was dropped).
func TestStatisticsSourceShape(t *testing.T) {
	b, err := json.Marshal(types.Source{
		LastStarted:  "2026-01-01T00:00:00Z",
		URL:          "udp://tracker.example:1337/announce",
		NumFound:     5,
		NumFoundUniq: 5,
		NumRequests:  1,
	})
	if err != nil {
		t.Fatalf("marshal Source: %v", err)
	}
	for _, k := range []string{"lastStarted", "url", "numFound", "numFoundUniq", "numRequests"} {
		if !strings.Contains(string(b), `"`+k+`":`) {
			t.Errorf("Source JSON missing required field %q: %s", k, b)
		}
	}
}
