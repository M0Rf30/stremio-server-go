package engine

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// TestAnnounceableTrackers guards the fix for the panic that anacrolix raises
// when a ws/wss tracker reaches its regular announce dispatcher (which happens
// once WebTorrent is disabled and the websocket announcer is gone):
// tracker.NewClient → ErrBadScheme → panicif.Err.
func TestAnnounceableTrackers(t *testing.T) {
	in := []string{
		"udp://tracker.opentrackr.org:1337/announce",
		"http://t.example/announce",
		"https://t.example/announce",
		"udp4://t4.example:6969/announce",
		"udp6://t6.example:6969/announce",
		"  WSS://tracker.openwebtorrent.com  ", // case + whitespace
		"ws://tracker.example",
		"dht://nodes", // unknown scheme must always be dropped
		"",            // empty must always be dropped
	}

	// WebTorrent disabled: ws/wss and unknown schemes dropped.
	gotOff := announceableTrackers(in, false)
	wantOff := []string{
		"udp://tracker.opentrackr.org:1337/announce",
		"http://t.example/announce",
		"https://t.example/announce",
		"udp4://t4.example:6969/announce",
		"udp6://t6.example:6969/announce",
	}
	if !reflect.DeepEqual(gotOff, wantOff) {
		t.Errorf("allowWS=false:\n got  %q\n want %q", gotOff, wantOff)
	}

	// WebTorrent enabled: ws/wss kept (verbatim), unknown still dropped.
	gotOn := announceableTrackers(in, true)
	wantOn := append(append([]string{}, wantOff...),
		"  WSS://tracker.openwebtorrent.com  ", "ws://tracker.example")
	if !reflect.DeepEqual(gotOn, wantOn) {
		t.Errorf("allowWS=true:\n got  %q\n want %q", gotOn, wantOn)
	}
}

// TestAnnounceableTiers verifies per-tier filtering drops tiers left empty.
func TestAnnounceableTiers(t *testing.T) {
	in := [][]string{
		{"wss://only.websocket"},                         // -> dropped (empty after filter)
		{"udp://keep.me:1337/announce", "wss://drop.me"}, // -> {udp...}
		{"http://a/announce", "https://b/announce"},      // -> both kept
	}
	got := announceableTiers(in, false)
	want := [][]string{
		{"udp://keep.me:1337/announce"},
		{"http://a/announce", "https://b/announce"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("announceableTiers:\n got  %q\n want %q", got, want)
	}
}

// TestParseTrackers verifies that parseTrackers accepts only known URL schemes
// and correctly trims whitespace, skips blanks, and rejects unknown protocols.
func TestParseTrackers(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name: "all known schemes",
			input: "udp://a.example:6969/announce\n" +
				"http://b.example/announce\n" +
				"https://c.example/announce\n" +
				"ws://d.example\n" +
				"wss://e.example\n",
			want: []string{
				"udp://a.example:6969/announce",
				"http://b.example/announce",
				"https://c.example/announce",
				"ws://d.example",
				"wss://e.example",
			},
		},
		{
			name:  "skips blank lines and unknown schemes",
			input: "\nftp://bad.example\ndht://bad\n\nudp://ok.example:1337/announce\n",
			want:  []string{"udp://ok.example:1337/announce"},
		},
		{
			name: "trims surrounding whitespace from each line",
			input: "  udp://trimmed.example:6969/announce  \n" +
				"  http://also-trimmed.example/announce  ",
			want: []string{
				"udp://trimmed.example:6969/announce",
				"http://also-trimmed.example/announce",
			},
		},
		{name: "empty input", input: "", want: nil},
		{name: "only blank lines", input: "\n\n\n", want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseTrackers(tc.input)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseTrackers:\n got  %q\n want %q", got, tc.want)
			}
		})
	}
}

// TestDedup verifies that dedup removes duplicate entries while preserving the
// first-occurrence order of every unique element.
func TestDedup(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "no duplicates",
			in:   []string{"a", "b", "c"},
			want: []string{"a", "b", "c"},
		},
		{
			name: "removes duplicates preserves order",
			in:   []string{"a", "b", "a", "c", "b"},
			want: []string{"a", "b", "c"},
		},
		{
			name: "all same",
			in:   []string{"x", "x", "x"},
			want: []string{"x"},
		},
		{
			name: "nil input",
			in:   nil,
			want: []string{},
		},
		{
			name: "empty slice",
			in:   []string{},
			want: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dedup(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("dedup:\n got  %q\n want %q", got, tc.want)
			}
		})
	}
}

// TestProbeTrackerUnknownScheme verifies that probeTracker returns probeMaxRTT
// for schemes it cannot probe (wss, dht, ftp, etc.) without making any network
// calls. This is the pure dispatch branch; actual UDP/HTTP prober tests would
// require a live tracker.
func TestProbeTrackerUnknownScheme(t *testing.T) {
	unknown := []string{
		"wss://tracker.openwebtorrent.com",
		"ws://tracker.example.com",
		"dht://nodes",
		"ftp://ftp.example.com",
		"magnet:?xt=urn:btih:abc",
	}
	for _, u := range unknown {
		t.Run(u, func(t *testing.T) {
			rtt := probeTracker(u)
			if rtt != probeMaxRTT {
				t.Errorf("probeTracker(%q) = %v; want probeMaxRTT (%v)", u, rtt, probeMaxRTT)
			}
		})
	}
}

// TestMergeWS verifies that mergeWS deduplicates input and appends the embedded
// wss:// trackers so that WebRTC peers remain discoverable even when the fetched
// UDP-only list contains no wss entries.
func TestMergeWS(t *testing.T) {
	in := []string{
		"udp://tracker1.example:6969/announce",
		"http://tracker2.example/announce",
	}
	got := mergeWS(in)

	// Every input entry must be preserved.
	for _, u := range in {
		found := false
		for _, g := range got {
			if g == u {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("mergeWS: input entry %q not found in result", u)
		}
	}

	// At least one embedded wss:// tracker must have been appended.
	wsCount := 0
	for _, g := range got {
		if strings.HasPrefix(g, "wss://") {
			wsCount++
		}
	}
	if wsCount == 0 {
		t.Error("mergeWS: expected at least one embedded wss:// tracker in result")
	}

	// No duplicates in the result.
	seen := make(map[string]int)
	for _, g := range got {
		seen[g]++
		if seen[g] > 1 {
			t.Errorf("mergeWS: duplicate entry %q in result", g)
		}
	}
}

// TestSetTrackers verifies that setTrackers ignores empty/nil slices (leaving
// the current list unchanged) and updates the list for non-empty input.
func TestSetTrackers(t *testing.T) {
	orig := getTrackers()
	t.Cleanup(func() { setTrackers(orig) })

	// nil and empty must not change the current list.
	setTrackers(nil)
	if got := getTrackers(); !reflect.DeepEqual(got, orig) {
		t.Error("setTrackers(nil): must not change currentTrackers")
	}
	setTrackers([]string{})
	if got := getTrackers(); !reflect.DeepEqual(got, orig) {
		t.Error("setTrackers(empty): must not change currentTrackers")
	}

	// Non-empty slice replaces the list.
	want := []string{"udp://test.example:6969/announce"}
	setTrackers(want)
	if got := getTrackers(); !reflect.DeepEqual(got, want) {
		t.Errorf("setTrackers: got %v, want %v", got, want)
	}
}

// TestProbeTrackerHTTPHeadSuccess verifies that probeTrackerHTTP returns a
// sub-probeMaxRTT duration when a HEAD request succeeds. Uses a local httptest
// server so no real network calls are made. Also exercises the probeTracker
// dispatch for the http:// scheme.
func TestProbeTrackerHTTPHeadSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rtt := probeTrackerHTTP(srv.URL)
	if rtt == probeMaxRTT {
		t.Errorf("probeTrackerHTTP HEAD success: got probeMaxRTT, want a short RTT")
	}

	// probeTracker dispatches http:// to probeTrackerHTTP.
	rtt2 := probeTracker(srv.URL)
	if rtt2 == probeMaxRTT {
		t.Errorf("probeTracker(http://...): got probeMaxRTT, want a short RTT")
	}
}

// TestProbeTrackerHTTPBothFail verifies that probeTrackerHTTP returns
// probeMaxRTT when the host is not reachable (connection refused). On localhost
// this fails instantly so the test does not stall.
func TestProbeTrackerHTTPBothFail(t *testing.T) {
	// Start and immediately stop a server to get a port that is now closed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // port is now closed → connection refused on both HEAD and GET

	rtt := probeTrackerHTTP(url)
	if rtt != probeMaxRTT {
		t.Errorf("probeTrackerHTTP unreachable host: got %v, want probeMaxRTT", rtt)
	}
}

// TestProbeTrackerUDPEmptyHost covers the empty-hostPort early return in
// probeTrackerUDP. A URL like "udp:///announce" has nothing between the
// scheme and the path, so hostPort becomes "" and probeMaxRTT is returned
// without any dial or I/O.
func TestProbeTrackerUDPEmptyHost(t *testing.T) {
	rtt := probeTrackerUDP("udp:///announce")
	if rtt != probeMaxRTT {
		t.Errorf("probeTrackerUDP empty host: got %v, want probeMaxRTT", rtt)
	}

	// probeTracker dispatches udp:// to probeTrackerUDP.
	rtt2 := probeTracker("udp:///announce")
	if rtt2 != probeMaxRTT {
		t.Errorf("probeTracker(udp:///announce): got %v, want probeMaxRTT", rtt2)
	}
}

// TestProbeTrackerUDPDialError covers the DialTimeout error path in
// probeTrackerUDP. A URL whose host part has no port causes
// net.DialTimeout to fail immediately (parse error, no network I/O) and
// the function must return probeMaxRTT.
func TestProbeTrackerUDPDialError(t *testing.T) {
	// "no-port" has no ':port' suffix → net.DialTimeout returns
	// "missing port in address" immediately without touching the network.
	rtt := probeTrackerUDP("udp://no-port-in-this-host/announce")
	if rtt != probeMaxRTT {
		t.Errorf("probeTrackerUDP dial error: got %v, want probeMaxRTT", rtt)
	}
}
