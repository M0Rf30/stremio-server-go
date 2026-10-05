// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"math"
	"testing"
)

func TestValidSeekSecs(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1, 1e12} {
		if validSeekSecs(v) {
			t.Errorf("%v must be rejected", v)
		}
	}
	if !validSeekSecs(0) || !validSeekSecs(3600) {
		t.Error("sane values must pass")
	}
	if got := secsToHHMMSS(math.Inf(1)); got != "100:00:00" {
		t.Errorf("got %s", got)
	}
	if got := secsToHHMMSS(math.NaN()); got != "00:00:00" {
		t.Errorf("got %s", got)
	}
}
