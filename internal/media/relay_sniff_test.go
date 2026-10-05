// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package media

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMediaRelayRewritesSniffedPlaylist: an HLS playlist served as
// octet-stream without a .m3u8 path must still be rewritten through the relay.
func TestMediaRelayRewritesSniffedPlaylist(t *testing.T) {
	const pl = "\xef\xbb\xbf\n#EXTM3U\n#EXTINF:4,\nhttp://169.254.169.254/latest/seg.ts\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(pl))
	}))
	defer upstream.Close()

	r := newMediaRelay(false)
	relayURL, err := r.register(upstream.URL + "/stream")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	resp, err := http.Get(relayURL) //nolint:gosec
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if strings.Contains(string(got), "169.254.169.254") || !strings.Contains(string(got), "/r/") {
		t.Errorf("nested URI not rewritten: %q", got)
	}
}

// TestMediaRelayRefusesDASH: DASH manifests are not rewritable, so refuse.
func TestMediaRelayRefusesDASH(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(`<?xml version="1.0"?><MPD><BaseURL>http://169.254.169.254/</BaseURL></MPD>`))
	}))
	defer upstream.Close()

	r := newMediaRelay(false)
	relayURL, err := r.register(upstream.URL + "/x")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	resp, err := http.Get(relayURL) //nolint:gosec
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", resp.StatusCode)
	}
}

func TestParseProbeOutputRejectsAbsurdDuration(t *testing.T) {
	for _, d := range []string{"NaN", "Inf", "-5", "0", "1e12"} {
		res := parseProbeOutput([]byte(`{"format":{"duration":"` + d + `"},"streams":[]}`))
		if res.duration != 0 {
			t.Errorf("duration %q parsed to %v, want 0", d, res.duration)
		}
	}
	if res := parseProbeOutput([]byte(`{"format":{"duration":"120.5"},"streams":[]}`)); res.duration != 120.5 {
		t.Errorf("valid duration = %v", res.duration)
	}
}
