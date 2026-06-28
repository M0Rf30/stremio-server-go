package types

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestOptionsJSONShape locks the stats.opts wire shape: the typed Options must
// marshal to exactly the keys stremio-core's strict Statistics.opts expects,
// with optional numeric fields rendered as JSON null (matching the legacy
// map[string]any that this struct replaced).
func TestOptionsJSONShape(t *testing.T) {
	b, err := json.Marshal(Options{
		DHT:        true,
		Path:       "/cache",
		PeerSearch: PeerSearch{Min: 40, Max: 200, Sources: []string{"dht:abc"}},
		Tracker:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	want := []string{
		`"connections":null`,
		`"dht":true`,
		`"growler":{"flood":0,"pulse":null}`,
		`"handshakeTimeout":null`,
		`"path":"/cache"`,
		`"peerSearch":{"min":40,"max":200,"sources":["dht:abc"]}`,
		`"swarmCap":{"maxSpeed":null,"minPeers":null}`,
		`"timeout":null`,
		`"tracker":true`,
		`"virtual":false`,
	}
	for _, sub := range want {
		if !strings.Contains(got, sub) {
			t.Errorf("opts JSON missing %s\n got: %s", sub, got)
		}
	}
}

// TestStatsJSONShape locks the Stats wire shape for the three fields that were
// previously typed as interface{}: Selections, Sources, and Opts. The JSON
// output of the typed fields MUST be byte-identical to what the former
// interface{} fields produced for the same underlying values.
func TestStatsJSONShape(t *testing.T) {
	src := Source{
		LastStarted:  "2024-01-01T00:00:00Z",
		URL:          "udp://tracker.example.com:1337",
		NumFound:     5,
		NumFoundUniq: 5,
		NumRequests:  1,
	}
	opts := Options{
		DHT:        true,
		Path:       "/cache",
		PeerSearch: PeerSearch{Min: 40, Max: 200, Sources: []string{"dht:abc"}},
		Tracker:    true,
	}
	st := Stats{
		InfoHash:          "aabbccdd",
		Name:              "test",
		Selections:        []any{},
		Wires:             []Wire{},
		Files:             []FileInfo{},
		Sources:           []Source{src},
		Opts:              opts,
		PeerSearchRunning: true,
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)

	// Selections typed as []any{} must produce an empty JSON array, not null.
	// Selections typed as nil must produce null — tested separately below.
	want := []string{
		`"selections":[]`,
		`"sources":[{"lastStarted":"2024-01-01T00:00:00Z","url":"udp://tracker.example.com:1337","numFound":5,"numFoundUniq":5,"numRequests":1}]`,
		`"opts":{"connections":null,"dht":true,"growler":{"flood":0,"pulse":null},"handshakeTimeout":null,"path":"/cache","peerSearch":{"min":40,"max":200,"sources":["dht:abc"]},"swarmCap":{"maxSpeed":null,"minPeers":null},"timeout":null,"tracker":true,"virtual":false}`,
		`"peerSearchRunning":true`,
	}
	for _, sub := range want {
		if !strings.Contains(got, sub) {
			t.Errorf("stats JSON missing %s\n got: %s", sub, got)
		}
	}

	// Nil Selections must marshal as null (not omitted — the field has no omitempty).
	st2 := Stats{Selections: nil}
	b2, err := json.Marshal(st2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b2), `"selections":null`) {
		t.Errorf("nil Selections should marshal as null, got: %s", string(b2))
	}
}
