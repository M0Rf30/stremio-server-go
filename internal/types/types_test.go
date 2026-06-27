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
