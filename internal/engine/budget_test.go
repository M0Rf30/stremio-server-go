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

// TestMinInt and TestMaxInt exercise the package-local generic min/max helpers.
// These are distinct from the Go 1.21 builtins: within package engine the
// package-level declarations take precedence.
func TestMinInt(t *testing.T) {
	cases := []struct{ a, b, want int }{
		{3, 5, 3},
		{5, 3, 3},
		{7, 7, 7},
		{0, -1, -1},
	}
	for _, tc := range cases {
		if got := min(tc.a, tc.b); got != tc.want {
			t.Errorf("min(%d, %d) = %d; want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestMaxInt(t *testing.T) {
	cases := []struct{ a, b, want int }{
		{3, 5, 5},
		{5, 3, 5},
		{7, 7, 7},
		{0, -1, 0},
	}
	for _, tc := range cases {
		if got := max(tc.a, tc.b); got != tc.want {
			t.Errorf("max(%d, %d) = %d; want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
