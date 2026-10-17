// SPDX-FileCopyrightText: 2026 The rivulet-engine Authors
//
// SPDX-License-Identifier: MIT

package engine_test

import (
	"strings"
	"testing"

	"github.com/rivulet-kodi/rivulet-engine/internal/engine"
	"github.com/rivulet-kodi/rivulet-engine/internal/types"
)

// TestStatsRealOpts verifies that stats.opts reports real connection/timeout
// values instead of the nulls the engine used to emit, so the fields carry the
// same meaning as the official server's getStatistics().
func TestStatsRealOpts(t *testing.T) {
	cfg := newTestCfg(t)
	cfg.PeersPerTorrent = 0 // default budget → 50 established
	em, err := engine.New(cfg)
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

	if s.Opts.Connections == nil {
		t.Error("Stats.Opts.Connections = nil; want the configured per-torrent budget")
	} else if *s.Opts.Connections <= 0 {
		t.Errorf("Stats.Opts.Connections = %d; want > 0", *s.Opts.Connections)
	}
	if s.Opts.HandshakeTimeout == nil {
		t.Error("Stats.Opts.HandshakeTimeout = nil; want a real timeout value")
	}
	if s.Opts.Timeout == nil {
		t.Error("Stats.Opts.Timeout = nil; want a real timeout value")
	}
	// PeerSearch must always be populated (the source list drives peer discovery).
	if s.Opts.PeerSearch.Max == 0 {
		t.Errorf("Stats.Opts.PeerSearch.Max = %d; want > 0", s.Opts.PeerSearch.Max)
	}
	// No soft limit is configured, so peer discovery runs and the torrent is not
	// paused. (swarmPaused and peerSearchRunning are two views of the same state,
	// so the useful assertion is the default value, not that they agree.)
	if s.SwarmPaused {
		t.Error("Stats.SwarmPaused = true with no soft limit configured")
	}
	if !s.PeerSearchRunning {
		t.Error("Stats.PeerSearchRunning = false with no soft limit configured")
	}
}

// TestStatsSourcesTrackerPrefix verifies that tracker announce URLs are reported
// with the "tracker:" prefix the official server uses, so duplicate URLs are not
// ambiguous and the source shape matches stremio-core's expectations.
func TestStatsSourcesTrackerPrefix(t *testing.T) {
	em, err := engine.New(newTestCfg(t))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer em.Close()

	e, err := em.EnsureEngine(knownHash, types.AddOptions{
		Trackers: []string{"udp://tracker.example.org:1337/announce"},
	})
	if err != nil {
		t.Fatalf("EnsureEngine: %v", err)
	}
	s := e.Stats(-1)
	if len(s.Sources) == 0 {
		t.Fatal("Stats.Sources is empty; want the provided tracker to be reported")
	}
	for _, src := range s.Sources {
		if !strings.HasPrefix(src.URL, "tracker:") {
			t.Errorf("Stats.Sources url = %q; want a \"tracker:\" prefix", src.URL)
		}
		if src.NumRequests < 1 {
			t.Errorf("Stats.Sources[%s].NumRequests = %d; want >= 1", src.URL, src.NumRequests)
		}
	}
}
