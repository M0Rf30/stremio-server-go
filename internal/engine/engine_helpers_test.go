package engine

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/M0Rf30/stremio-server-go/internal/types"
)

// TestTrackersFromTorrentJSON covers every branch of the pure JSON-extraction
// helper: nil input, parse error, empty object, announce-only, list-only, and
// both fields present.
func TestTrackersFromTorrentJSON(t *testing.T) {
	cases := []struct {
		name string
		raw  json.RawMessage
		want []string
	}{
		{
			name: "nil input returns nil",
			raw:  nil,
			want: nil,
		},
		{
			name: "invalid JSON returns nil",
			raw:  json.RawMessage(`not valid json`),
			want: nil,
		},
		{
			name: "empty object returns nil",
			raw:  json.RawMessage(`{}`),
			want: nil,
		},
		{
			name: "announce only",
			raw:  json.RawMessage(`{"announce":"udp://tracker.example:6969/announce"}`),
			want: []string{"udp://tracker.example:6969/announce"},
		},
		{
			name: "announce-list only, multiple tiers",
			raw: json.RawMessage(`{"announce-list":[` +
				`["udp://a.example:6969/announce"],` +
				`["http://b.example/announce","https://c.example/announce"]` +
				`]}`),
			want: []string{
				"udp://a.example:6969/announce",
				"http://b.example/announce",
				"https://c.example/announce",
			},
		},
		{
			name: "announce + announce-list, announce is prepended",
			raw: json.RawMessage(`{` +
				`"announce":"udp://first.example:6969/announce",` +
				`"announce-list":[["udp://second.example:6969/announce"]]` +
				`}`),
			want: []string{
				"udp://first.example:6969/announce",
				"udp://second.example:6969/announce",
			},
		},
		{
			// announce field is empty string → treated as absent.
			name: "empty announce string, non-empty list",
			raw:  json.RawMessage(`{"announce":"","announce-list":[["udp://x.example:6969"]]}`),
			want: []string{"udp://x.example:6969"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := trackersFromTorrentJSON(tc.raw)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("trackersFromTorrentJSON:\n got  %v\n want %v", got, tc.want)
			}
		})
	}
}

// TestNumTorrents verifies that NumTorrents reflects live engine count.
// It is a method on *manager (not in the EngineManager interface), so it must
// be tested from an internal test that can assert the concrete type.
func TestNumTorrents(t *testing.T) {
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
	defer func() { _ = em.Close() }()
	m := em.(*manager)

	if n := m.NumTorrents(); n != 0 {
		t.Errorf("initial NumTorrents = %d; want 0", n)
	}

	const ih = "08ada5a7a6183aae1e09d831df6748d566095a10"
	if _, err := m.EnsureEngine(ih, types.AddOptions{}); err != nil {
		t.Fatalf("EnsureEngine: %v", err)
	}
	if n := m.NumTorrents(); n != 1 {
		t.Errorf("after EnsureEngine NumTorrents = %d; want 1", n)
	}
}

// TestEvictBudgetZero verifies the early return in evict when budget ≤ 0
// (unlimited cache) and when the manager has no engines (empty snapshot).
// Neither call must panic or change engine state.
func TestEvictBudgetZero(t *testing.T) {
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
	defer func() { _ = em.Close() }()
	m := em.(*manager)

	// budget ≤ 0 → immediate return; no engines must be touched.
	m.evict(0)
	m.evict(-1)

	// budget > 0 with no engines → snap is empty → early return.
	m.evict(1024)

	if m.NumTorrents() != 0 {
		t.Error("evict: expected 0 engines after no-op calls")
	}
}
