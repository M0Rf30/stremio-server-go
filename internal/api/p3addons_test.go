// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"testing"

	"github.com/M0Rf30/stremio-server-go/internal/archive"
)

// A video entry whose size is unknown (streamed RAR, Size 0) must still be
// selectable, and a larger known video must beat it.
func TestArchiveLargestVideoUnknownSize(t *testing.T) {
	got := archiveLargestVideo([]archive.Entry{
		{Name: "a.txt", Size: 10},
		{Name: "movie.mkv", SizeUnknown: true},
	})
	if got != "movie.mkv" {
		t.Fatalf("unknown-size video not selected: %q", got)
	}
	got = archiveLargestVideo([]archive.Entry{
		{Name: "movie.mkv", SizeUnknown: true},
		{Name: "sample.mp4", Size: 5},
	})
	if got != "sample.mp4" {
		t.Fatalf("known larger video should win: %q", got)
	}
}

// With every IMDB slot busy, ensureIMDBResolved must not spawn a goroutine or
// mark the file pending, so it is retried on a later scan.
func TestEnsureIMDBResolvedSaturatedSpawnsNothing(t *testing.T) {
	prev := localIMDBDisabled.Load()
	localIMDBDisabled.Store(false)
	defer localIMDBDisabled.Store(prev)

	for range cap(imdbSem) {
		imdbSem <- struct{}{}
	}
	defer func() {
		for range cap(imdbSem) {
			<-imdbSem
		}
	}()

	const hex = "p3addons-saturated"
	ensureIMDBResolved(hex, "Some Title", 2020, "movie", "/tmp/x.mkv")

	imdbPendMu.Lock()
	pending := imdbPending[hex]
	imdbPendMu.Unlock()
	if pending {
		t.Fatal("file marked pending although no slot was available")
	}
}
