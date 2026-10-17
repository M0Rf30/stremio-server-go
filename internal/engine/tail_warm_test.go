// SPDX-FileCopyrightText: 2026 The rivulet-engine Authors
//
// SPDX-License-Identifier: MIT

package engine

import (
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/rivulet-kodi/rivulet-engine/internal/types"
)

// newTailWarmTestEngine builds an engine with a real single-file torrent whose
// info is ready, so the abandon path can run its piece demotion without panics.
func newTailWarmTestEngine(t *testing.T) *engine {
	t.Helper()
	info := &metainfo.Info{
		Name:        "tailtest",
		PieceLength: 512,
		Length:      4 * 512,
		Pieces:      make([]byte, metainfo.HashSize*4),
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatalf("marshal info: %v", err)
	}
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
	tor, err := em.(*manager).client.AddTorrent(&metainfo.MetaInfo{InfoBytes: infoBytes})
	if err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}
	<-tor.GotInfo()
	return &engine{t: tor, infoHash: "tailtest"}
}

// TestAbandonTailWarmClearsMarker verifies the once-only tail-warm marker is
// cleared when the pre-read gives up, so a later NewReader can retry instead of
// the tail staying requested forever.
func TestAbandonTailWarmClearsMarker(t *testing.T) {
	e := newTailWarmTestEngine(t)

	e.mu.Lock()
	e.tailWarmed = map[int]struct{}{0: {}}
	e.mu.Unlock()

	e.abandonTailWarm(0)

	e.mu.Lock()
	_, warmed := e.tailWarmed[0]
	e.mu.Unlock()
	if warmed {
		t.Error("tailWarmed not cleared; a later stream could not re-warm the tail")
	}
}

// TestTailWarmWanted pins the demotion decision: the tail is only dropped when
// neither a live reader nor a full-file selection wants the file. Both pieces of
// state are read under one lock (the caller holds e.mu).
func TestTailWarmWanted(t *testing.T) {
	e := newTailWarmTestEngine(t)

	tests := []struct {
		name     string
		reading  int
		selected bool
		want     bool
	}{
		{"no reader, no selection", 0, false, false},
		{"live reader", 1, false, true},
		{"selected for background download", 0, true, true},
		{"both", 2, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e.mu.Lock()
			e.reading = map[int]int{0: tc.reading}
			if tc.selected {
				e.selected = map[int]struct{}{0: {}}
			} else {
				e.selected = map[int]struct{}{}
			}
			got := e.tailWarmWantedLocked(0)
			e.mu.Unlock()
			if got != tc.want {
				t.Errorf("tailWarmWantedLocked = %v; want %v", got, tc.want)
			}
		})
	}
}
