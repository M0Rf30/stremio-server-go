package engine

import "testing"

func TestPeerBudget(t *testing.T) {
	cases := []struct {
		n                                int
		wantEst, wantHalf, wantHighWater int
	}{
		{0, 50, 25, 500},
		{30, 30, 15, 300},
		{10, 10, 5, 100},
		{-5, 50, 25, 500},
	}
	for _, tc := range cases {
		est, half, high := peerBudget(tc.n)
		if est != tc.wantEst || half != tc.wantHalf || high != tc.wantHighWater {
			t.Errorf("peerBudget(%d) = (%d,%d,%d); want (%d,%d,%d)",
				tc.n, est, half, high, tc.wantEst, tc.wantHalf, tc.wantHighWater)
		}
	}
}
