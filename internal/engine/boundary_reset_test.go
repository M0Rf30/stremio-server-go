// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package engine

import (
	"reflect"
	"testing"

	"github.com/anacrolix/torrent"
)

type recSetter struct{ calls map[int]torrent.PiecePriority }

func (r *recSetter) set(i int, p torrent.PiecePriority) {
	if r.calls == nil {
		r.calls = map[int]torrent.PiecePriority{}
	}
	r.calls[i] = p
}

// TestRaiseBoundaryNeverLowers: a Normal prefetch over a piece already at Now
// must not call SetPriority on it (anacrolix would overwrite Now with Normal).
func TestRaiseBoundaryNeverLowers(t *testing.T) {
	e := &engine{}
	var rec recSetter
	e.raiseBoundary([]pieceSpan{{5, 6}}, torrent.PiecePriorityNow, rec.set)
	rec = recSetter{}
	e.raiseBoundary([]pieceSpan{{4, 7}}, torrent.PiecePriorityNormal, rec.set)
	if _, touched := rec.calls[5]; touched {
		t.Fatalf("shared piece 5 at Now was re-set: %v", rec.calls)
	}
	if rec.calls[4] != torrent.PiecePriorityNormal || rec.calls[6] != torrent.PiecePriorityNormal {
		t.Fatalf("unset pieces must be raised to Normal: %v", rec.calls)
	}
	// Raising Normal -> Now still works.
	rec = recSetter{}
	e.raiseBoundary([]pieceSpan{{4, 5}}, torrent.PiecePriorityNow, rec.set)
	if rec.calls[4] != torrent.PiecePriorityNow {
		t.Fatalf("Normal->Now raise missing: %v", rec.calls)
	}
}

// TestPiecesToReleaseSkipsShared: pieces covered by a still-active file's
// boundary are kept; the rest of the demoted file's boundary is released.
func TestPiecesToReleaseSkipsShared(t *testing.T) {
	demoted := []pieceSpan{{0, 2}, {8, 10}}
	protected := []pieceSpan{{9, 12}}
	got := piecesToRelease(demoted, protected)
	if want := []int{0, 1, 8}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// Overlapping head/tail spans yield each piece once.
	if got := piecesToRelease([]pieceSpan{{0, 3}, {2, 4}}, nil); !reflect.DeepEqual(got, []int{0, 1, 2, 3}) {
		t.Fatalf("dedup: got %v", got)
	}
}

// TestReleasePiecesResetsOnlyRaised: released pieces go back to None and are
// forgotten, so a later prefetch/prime can raise them again; pieces we never
// raised are not touched.
func TestReleasePiecesResetsOnlyRaised(t *testing.T) {
	e := &engine{}
	var rec recSetter
	e.raiseBoundary([]pieceSpan{{0, 2}}, torrent.PiecePriorityNow, rec.set)

	rec = recSetter{}
	e.boundaryMu.Lock()
	e.releasePieces([]int{0, 1, 7}, rec.set)
	e.boundaryMu.Unlock()
	if len(rec.calls) != 2 || rec.calls[0] != torrent.PiecePriorityNone || rec.calls[1] != torrent.PiecePriorityNone {
		t.Fatalf("reset calls: %v", rec.calls)
	}

	rec = recSetter{}
	e.raiseBoundary([]pieceSpan{{0, 1}}, torrent.PiecePriorityNormal, rec.set)
	if rec.calls[0] != torrent.PiecePriorityNormal {
		t.Fatalf("piece must be raisable again after release: %v", rec.calls)
	}
}

func TestBoundarySpans(t *testing.T) {
	got := boundarySpans(10, 100, 16<<20)
	if want := []pieceSpan{{10, 11}, {99, 100}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
