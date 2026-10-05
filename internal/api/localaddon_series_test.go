// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFilenameSeriesEpisode(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		title   string
		season  int
		episode int
	}{
		{
			name:    "standard SxxExx",
			input:   "Breaking Bad S01E05 720p",
			title:   "Breaking Bad",
			season:  1,
			episode: 5,
		},
		{
			name:    "alternate x notation",
			input:   "Breaking Bad 2x12 1080p",
			title:   "Breaking Bad",
			season:  2,
			episode: 12,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseFilenameToMeta(tt.input)

			if got.ctype != "series" {
				t.Fatalf("ctype = %q, want series", got.ctype)
			}
			if got.name != tt.title {
				t.Errorf("name = %q, want %q", got.name, tt.title)
			}
			if got.season != tt.season {
				t.Errorf("season = %d, want %d", got.season, tt.season)
			}
			if got.episode != tt.episode {
				t.Errorf("episode = %d, want %d", got.episode, tt.episode)
			}
		})
	}
}

func TestDedupeLocalSeriesCatalogPrefersResolvedID(t *testing.T) {
	key := normalizeSeriesKey("Example Show")
	localID := localSeriesID(key)
	items := []localMeta{
		{
			ID:        localID,
			Name:      "Example Show",
			Type:      "series",
			SeriesKey: key,
			Season:    1,
			Episode:   3,
		},
		{
			ID:        "tt1234567",
			Name:      "Example Show",
			Type:      "series",
			SeriesKey: key,
			Season:    1,
			Episode:   4,
		},
		{
			ID:   "tt9999999",
			Name: "Example Movie",
			Type: "movie",
		},
	}

	got := dedupeLocalMetas(items, "series")

	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].ID != "tt1234567" {
		t.Fatalf("id = %q, want tt1234567", got[0].ID)
	}
}

func TestLocalSeriesVideosIncludeUnresolvedEpisodes(t *testing.T) {
	key := normalizeSeriesKey("Example Show")
	localID := localSeriesID(key)
	items := []localMeta{
		{
			ID:        "tt1234567",
			Name:      "Example Show",
			Type:      "series",
			SeriesKey: key,
			Season:    1,
			Episode:   3,
		},
		{
			ID:        localID,
			Name:      "Example Show",
			Type:      "series",
			SeriesKey: key,
			Season:    1,
			Episode:   4,
		},
	}

	got := localSeriesVideos(items, "tt1234567")

	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0]["id"] != "tt1234567:1:3" {
		t.Errorf("first id = %v, want tt1234567:1:3", got[0]["id"])
	}
	if got[1]["id"] != "tt1234567:1:4" {
		t.Errorf("second id = %v, want tt1234567:1:4", got[1]["id"])
	}
}

func TestLocalSeriesEpisodeFindsUnresolvedEpisodeFromResolvedID(t *testing.T) {
	key := normalizeSeriesKey("Example Show")
	localID := localSeriesID(key)
	items := []localMeta{
		{
			ID:        "tt1234567",
			LocalHex:  "aaa",
			Name:      "Example Show",
			Path:      "/media/Example.Show.S01E03.mkv",
			Type:      "series",
			SeriesKey: key,
			Season:    1,
			Episode:   3,
		},
		{
			ID:        localID,
			LocalHex:  "bbb",
			Name:      "Example Show",
			Path:      "/media/Example.Show.S01E04.mkv",
			Type:      "series",
			SeriesKey: key,
			Season:    1,
			Episode:   4,
		},
	}

	got, ok := localSeriesEpisode(items, "tt1234567:1:4")
	if !ok {
		t.Fatal("expected episode to be found")
	}
	if got.Path != "/media/Example.Show.S01E04.mkv" {
		t.Fatalf("path = %q, want S01E04 file", got.Path)
	}
}

func TestLocalSeriesEpisodeMissing(t *testing.T) {
	key := normalizeSeriesKey("Example Show")
	items := []localMeta{
		{
			ID:        localSeriesID(key),
			Name:      "Example Show",
			Type:      "series",
			SeriesKey: key,
			Season:    1,
			Episode:   3,
		},
	}

	_, ok := localSeriesEpisode(items, localSeriesID(key)+":1:99")
	if ok {
		t.Fatal("unexpected match for missing episode")
	}
}

func TestLocalSeriesGroupingWithoutIMDB(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"Show.S01E01.mkv",
		"show.S01E02.mkv",
		"Show_S01E03.mkv",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("LOCAL_FILES_DIR", dir)
	wasDisabled := localIMDBDisabled.Load()
	localIMDBDisabled.Store(true)
	t.Cleanup(func() {
		localIMDBDisabled.Store(wasDisabled)
	})

	items := scanLocalFiles()
	if len(items) != 3 {
		t.Fatalf("len(items) = %d, want 3", len(items))
	}

	wantKey := normalizeSeriesKey("Show")
	wantID := localSeriesID(wantKey)
	for _, m := range items {
		if m.SeriesKey != wantKey {
			t.Errorf("SeriesKey = %q, want %q", m.SeriesKey, wantKey)
		}
		if m.ID != wantID {
			t.Errorf("ID = %q, want shared series ID %q", m.ID, wantID)
		}
		if !strings.HasPrefix(m.ID, "local:") {
			t.Errorf("ID = %q, want local: prefix", m.ID)
		}
	}

	catalog := dedupeLocalMetas(items, "series")
	if len(catalog) != 1 {
		t.Fatalf("catalog len = %d, want 1", len(catalog))
	}
	if catalog[0].ID != wantID {
		t.Fatalf("catalog id = %q, want %q", catalog[0].ID, wantID)
	}

	videos := localSeriesVideos(items, wantID)
	if len(videos) != 3 {
		t.Fatalf("videos len = %d, want 3", len(videos))
	}

	episode, ok := localSeriesEpisode(items, wantID+":1:2")
	if !ok {
		t.Fatal("expected S01E02 to be found")
	}
	if filepath.Base(episode.Path) != "show.S01E02.mkv" {
		t.Fatalf("episode path = %q, want show.S01E02.mkv", episode.Path)
	}
}

func scanTree(t *testing.T, files ...string) []localMeta {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LOCAL_FILES_DIR", dir)
	wasDisabled := localIMDBDisabled.Load()
	localIMDBDisabled.Store(true)
	t.Cleanup(func() { localIMDBDisabled.Store(wasDisabled) })
	return scanLocalFiles()
}

func TestLocalSeriesBareFilenameUsesParentDir(t *testing.T) {
	items := scanTree(t, "A/S01E01.mkv", "B/S01E01.mkv", "C/1x02.mkv")
	if len(items) != 3 {
		t.Fatalf("len = %d, want 3", len(items))
	}
	names := map[string]string{}
	for _, m := range items {
		names[m.Name] = m.ID
	}
	for _, n := range []string{"A", "B", "C"} {
		if names[n] == "" {
			t.Errorf("missing series %q in %v", n, names)
		}
	}
	if names["A"] == names["B"] {
		t.Error("A and B collapsed into one series id")
	}
	if got := dedupeLocalMetas(items, "series"); len(got) != 3 {
		t.Errorf("catalog len = %d, want 3", len(got))
	}
}

func TestLocalSeriesBareFilenameSeasonDirUsesGrandparent(t *testing.T) {
	items := scanTree(t, "Show/Season 1/S01E02.mkv", "Show/S02/S02E01.mkv", "Show/Staffel 2/S02E03.mkv")
	if len(items) != 3 {
		t.Fatalf("len = %d, want 3", len(items))
	}
	for _, m := range items {
		if m.Name != "Show" {
			t.Errorf("name = %q, want Show", m.Name)
		}
		if m.ID != items[0].ID {
			t.Errorf("id = %q, want %q", m.ID, items[0].ID)
		}
	}
}

func TestLocalSeriesVideosSorted(t *testing.T) {
	key := normalizeSeriesKey("Example Show")
	id := localSeriesID(key)
	var items []localMeta
	for _, se := range [][2]int{{2, 1}, {1, 10}, {1, 2}, {2, 3}} {
		items = append(items, localMeta{ID: id, Type: "series", SeriesKey: key, Season: se[0], Episode: se[1]})
	}
	got := localSeriesVideos(items, id)
	want := []string{":1:2", ":1:10", ":2:1", ":2:3"}
	if len(got) != len(want) {
		t.Fatalf("len = %d", len(got))
	}
	for i, w := range want {
		if g, _ := got[i]["id"].(string); g != id+w {
			t.Errorf("videos[%d] = %v, want %s", i, got[i]["id"], id+w)
		}
	}
}
