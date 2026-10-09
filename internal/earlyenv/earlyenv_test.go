// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package earlyenv

import (
	"errors"
	"testing"
)

func TestApply(t *testing.T) {
	cases := []struct {
		name    string
		preset  bool
		setErr  error
		want    bool
		wantSet bool
	}{
		{"forces classic by default", false, nil, true, true},
		{"respects explicit choice", true, nil, false, false},
		{"setenv failure", false, errors.New("boom"), false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var setKey, setVal string
			lookup := func(string) (string, bool) { return "mmap", tc.preset }
			set := func(k, v string) error { setKey, setVal = k, v; return tc.setErr }
			if got := apply(lookup, set); got != tc.want {
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
