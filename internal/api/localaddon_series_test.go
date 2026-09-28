package api

import "testing"

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

func TestDedupeLocalSeriesCatalog(t *testing.T) {
	items := []localMeta{
		{
			ID:      "tt1234567",
			Name:    "Example Show",
			Type:    "series",
			Season:  1,
			Episode: 3,
		},
		{
			ID:      "tt1234567",
			Name:    "Example Show",
			Type:    "series",
			Season:  1,
			Episode: 4,
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

func TestLocalSeriesVideos(t *testing.T) {
	items := []localMeta{
		{
			ID:      "tt1234567",
			Name:    "Example Show",
			Type:    "series",
			Season:  1,
			Episode: 3,
		},
		{
			ID:      "tt1234567",
			Name:    "Example Show",
			Type:    "series",
			Season:  1,
			Episode: 4,
		},
		{
			ID:      "tt7654321",
			Name:    "Other Show",
			Type:    "series",
			Season:  1,
			Episode: 1,
		},
	}

	got := localSeriesVideos(items, "tt1234567")

	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}

	if got[0]["id"] != "tt1234567:1:3" {
		t.Errorf("first id = %v, want tt1234567:1:3", got[0]["id"])
	}
	if got[0]["season"] != 1 {
		t.Errorf("first season = %v, want 1", got[0]["season"])
	}
	if got[0]["episode"] != 3 {
		t.Errorf("first episode = %v, want 3", got[0]["episode"])
	}

	if got[1]["id"] != "tt1234567:1:4" {
		t.Errorf("second id = %v, want tt1234567:1:4", got[1]["id"])
	}
}

func TestLocalSeriesEpisode(t *testing.T) {
	items := []localMeta{
		{
			ID:       "tt1234567",
			LocalHex: "aaa",
			Name:     "Example Show",
			Path:     "/media/Example.Show.S01E03.mkv",
			Type:     "series",
			Season:   1,
			Episode:  3,
		},
		{
			ID:       "tt1234567",
			LocalHex: "bbb",
			Name:     "Example Show",
			Path:     "/media/Example.Show.S01E04.mkv",
			Type:     "series",
			Season:   1,
			Episode:  4,
		},
	}

	got, ok := localSeriesEpisode(items, "tt1234567:1:3")
	if !ok {
		t.Fatal("expected episode to be found")
	}
	if got.Path != "/media/Example.Show.S01E03.mkv" {
		t.Fatalf("path = %q, want S01E03 file", got.Path)
	}
}

func TestLocalSeriesEpisodeMissing(t *testing.T) {
	items := []localMeta{
		{
			ID:      "tt1234567",
			Name:    "Example Show",
			Type:    "series",
			Season:  1,
			Episode: 3,
		},
	}

	_, ok := localSeriesEpisode(items, "tt1234567:1:99")
	if ok {
		t.Fatal("unexpected match for missing episode")
	}
}
