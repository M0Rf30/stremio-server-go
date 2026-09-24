package earlyenv

import (
	"errors"
	"testing"
)

func TestApply(t *testing.T) {
	cases := []struct {
		name     string
		wordBits int
		preset   bool
		setErr   error
		want     bool
		wantSet  bool
	}{
		{"64-bit untouched", 64, false, nil, false, false},
		{"32-bit forces classic", 32, false, nil, true, true},
		{"32-bit respects explicit choice", 32, true, nil, false, false},
		{"32-bit setenv failure", 32, false, errors.New("boom"), false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var setKey, setVal string
			lookup := func(string) (string, bool) { return "mmap", tc.preset }
			set := func(k, v string) error { setKey, setVal = k, v; return tc.setErr }
			if got := apply(tc.wordBits, lookup, set); got != tc.want {
				t.Fatalf("apply() = %v, want %v", got, tc.want)
			}
			if tc.wantSet && (setKey != FileIoEnv || setVal != "classic") {
				t.Fatalf("set(%q, %q), want (%q, classic)", setKey, setVal, FileIoEnv)
			}
			if !tc.wantSet && setKey != "" {
				t.Fatalf("unexpected set(%q, %q)", setKey, setVal)
			}
		})
	}
}
