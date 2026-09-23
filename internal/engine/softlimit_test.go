package engine

import "testing"

// TestSoftLimitDecision exercises the btDownloadSpeedSoftLimit decision as a
// pure function: pause peer discovery only once the torrent already sustains
// the configured speed AND has enough connected peers to prove it doesn't
// need more (see manager.SetSoftLimitFn for the full official semantics).
func TestSoftLimitDecision(t *testing.T) {
	const mib = 1 << 20
	cases := []struct {
		name              string
		speed             float64
		softLimit         int64
		connectedPeers    int
		minPeersForStable int
		want              bool
	}{
		{"disabled (soft limit <= 0) never pauses", 100 * mib, 0, 50, 5, false},
		{"disabled (negative soft limit) never pauses", 100 * mib, -1, 50, 5, false},
		{"below speed threshold, plenty of peers: keep discovering", 1 * mib, 2.5 * mib, 10, 5, false},
		{"at/above threshold but too few peers: keep discovering", 3 * mib, 2.5 * mib, 4, 5, false},
		{"at threshold exactly, enough peers: pause", 2.5 * mib, 2.5 * mib, 5, 5, true},
		{"above threshold, enough peers: pause", 5 * mib, 2.5 * mib, 8, 5, true},
		{"above threshold, more than enough peers: pause", 10 * mib, 2.5 * mib, 20, 5, true},
		{"zero connected peers, threshold zero required: pause once speed reached", 3 * mib, 2.5 * mib, 0, 0, true},
		{"zero speed never pauses even with peers", 0, 2.5 * mib, 50, 5, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := softLimitDecision(tc.speed, tc.softLimit, tc.connectedPeers, tc.minPeersForStable)
			if got != tc.want {
				t.Errorf("softLimitDecision(speed=%v, soft=%d, peers=%d, minPeers=%d) = %v; want %v",
					tc.speed, tc.softLimit, tc.connectedPeers, tc.minPeersForStable, got, tc.want)
			}
		})
	}
}
