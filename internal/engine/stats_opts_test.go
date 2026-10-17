// SPDX-FileCopyrightText: 2026 The rivulet-engine Authors
//
// SPDX-License-Identifier: MIT

package engine

import (
	"testing"

	"github.com/anacrolix/torrent"

	"github.com/rivulet-kodi/rivulet-engine/internal/types"
)

func newStatsOptsManager(t *testing.T) *manager {
	t.Helper()
	em, err := New(types.Config{
		HTTPPort:          0,
		ListenPort:        0,
		AppPath:           t.TempDir(),
		CacheRoot:         t.TempDir(),
		Version:           "4.21.0",
		DisableTrackers:   true,
		DisableWebtorrent: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = em.Close() })
	return em.(*manager)
}

// TestStatsOptsTimeoutsFromClientConfig verifies opts.timeout and
// opts.handshakeTimeout come from the anacrolix client config (dial and
// handshake timeouts), not a hardcoded constant, and are reported in ms.
func TestStatsOptsTimeoutsFromClientConfig(t *testing.T) {
	m := newStatsOptsManager(t)
	cc := torrent.NewDefaultClientConfig()

	if m.handshakeTimeout != cc.HandshakesTimeout {
		t.Errorf("manager handshakeTimeout = %v; want %v", m.handshakeTimeout, cc.HandshakesTimeout)
	}
	if m.dialTimeout != cc.NominalDialTimeout {
		t.Errorf("manager dialTimeout = %v; want %v", m.dialTimeout, cc.NominalDialTimeout)
	}

	opts := m.statsOptsSource()
	if opts.HandshakeTimeout == nil {
		t.Fatal("opts.HandshakeTimeout = nil; want the client handshake timeout")
	}
	if got, want := *opts.HandshakeTimeout, int(cc.HandshakesTimeout.Milliseconds()); got != want {
		t.Errorf("opts.HandshakeTimeout = %d ms; want %d", got, want)
	}
	if opts.Timeout == nil {
		t.Fatal("opts.Timeout = nil; want the client dial timeout")
	}
	if got, want := *opts.Timeout, int(cc.NominalDialTimeout.Milliseconds()); got != want {
		t.Errorf("opts.Timeout = %d ms; want %d", got, want)
	}
}

// TestStatsOptsSwarmCapFollowsSoftLimit verifies swarmCap is absent without a
// soft limit and reflects a soft limit set after the manager was created (i.e.
// it is not frozen at engine creation).
func TestStatsOptsSwarmCapFollowsSoftLimit(t *testing.T) {
	m := newStatsOptsManager(t)

	if opts := m.statsOptsSource(); opts.SwarmCap.MaxSpeed != nil {
		t.Error("swarmCap set without a soft limit; want it omitted")
	}

	const softBytes = int64(4 << 20)
	m.SetSoftLimitFn(func() (int64, int) { return softBytes, 5 })

	opts := m.statsOptsSource()
	if opts.SwarmCap.MaxSpeed == nil || *opts.SwarmCap.MaxSpeed != float64(softBytes) {
		t.Errorf("swarmCap.maxSpeed = %v; want %d", opts.SwarmCap.MaxSpeed, softBytes)
	}
	if opts.SwarmCap.MinPeers == nil || *opts.SwarmCap.MinPeers != 5 {
		t.Errorf("swarmCap.minPeers = %v; want 5", opts.SwarmCap.MinPeers)
	}
}

// TestAccumulateConnTriesHeuristic pins the dial-churn heuristic: it only sums
// increases in half-open peers (drops do not subtract), so it never decreases.
func TestAccumulateConnTriesHeuristic(t *testing.T) {
	e := &engine{}
	steps := []struct {
		halfOpen int
		want     int
	}{
		{3, 3}, // first sample counts fully
		{1, 3}, // a drop does not subtract
		{4, 6}, // +3 new dials
		{4, 6}, // steady state
		{0, 6}, // another drop does not subtract
		{2, 8}, // +2
	}
	for _, s := range steps {
		if got := e.accumulateConnTries(s.halfOpen); got != s.want {
			t.Errorf("accumulateConnTries(%d) = %d; want %d", s.halfOpen, got, s.want)
		}
	}
}
