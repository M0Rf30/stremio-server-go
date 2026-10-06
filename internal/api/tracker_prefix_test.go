// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/M0Rf30/stremio-server-go/internal/types"
)

func TestStripTrackerPrefixes(t *testing.T) {
	in := []string{
		"tracker:udp://tracker.opentrackr.org:1337/announce", // Torrentio-style source
		"udp://bare.example.com:80/announce",                 // already a bare URL
		"dht:" + testIH,                                      // no announce URL: skipped
		"  tracker:  https://spaced.example.com/announce  ",  // whitespace around prefix and URL
		"tracker:", // prefix with nothing behind it
		"",
		"   ",
	}
	want := []string{
		"udp://tracker.opentrackr.org:1337/announce",
		"udp://bare.example.com:80/announce",
		"https://spaced.example.com/announce",
	}
	if got := stripTrackerPrefixes(in); !reflect.DeepEqual(got, want) {
		t.Errorf("stripTrackerPrefixes = %v; want %v", got, want)
	}
	if got := stripTrackerPrefixes(nil); len(got) != 0 {
		t.Errorf("nil input -> %v", got)
	}
}

// stremio-core forwards a stream's announce/sources entries verbatim as `tr`
// params, and Torrentio/AIOStreams-style sources are "tracker:"/"dht:"
// wrapped. The SSRF filter used to see "tracker:udp://…" as an opaque URL with
// no host and drop it, so the engine never learned the stream's own trackers.
func TestHandleStreamAcceptsTrackerPrefixedSources(t *testing.T) {
	em := &trackerSpyEM{fakeEM: newFakeEM(testEngine())}
	h := New(em, &fakeSS{}, &fakeProber{}, types.Config{HTTPPort: 11470})
	q := url.Values{"tr": {
		"tracker:udp://tracker.example.com:80/announce",
		"dht:" + testIH,
		"tracker:http://127.0.0.1:9/a", // still subject to the SSRF filter
		"udp://bare.example.com:80/announce",
	}}
	rec := serve(t, h, http.MethodGet, "/"+testIH+"/0?"+q.Encode(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	want := []string{
		"udp://tracker.example.com:80/announce",
		"udp://bare.example.com:80/announce",
	}
	if !reflect.DeepEqual(em.last.Trackers, want) {
		t.Errorf("trackers = %v; want %v", em.last.Trackers, want)
	}
}

// POST /{ih}/create carries trackers as peerSearch.sources ("tracker:<url>" /
// "dht:<hash>", the syntax stremio-core's CreateTorrentRequest produces); that
// path already normalised them and must keep doing so, including the SSRF filter.
func TestHandleCreatePeerSearchSourcesStayNormalised(t *testing.T) {
	em := &trackerSpyEM{fakeEM: newFakeEM(testEngine())}
	h := New(em, &fakeSS{}, &fakeProber{}, types.Config{HTTPPort: 11470})
	body := `{"peerSearch":{"min":40,"max":200,"sources":["dht:` + testIH + `","tracker:udp://b.example.com:80/announce","tracker:http://127.0.0.1:9/a"]}}`
	rec := serve(t, h, http.MethodPost, "/"+testIH+"/create", strings.NewReader(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", rec.Code, rec.Body.String())
	}
	want := []string{"udp://b.example.com:80/announce"}
	if !reflect.DeepEqual(em.last.Trackers, want) {
		t.Errorf("trackers = %v; want %v", em.last.Trackers, want)
	}
}
