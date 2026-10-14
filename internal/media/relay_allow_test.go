// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package media

// Tests for the relay's private-destination allowlist
// (STREMIO_HLS_PRIVATE_ALLOW: the relay and the pre-flight checks).

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/netguard"
	"github.com/M0Rf30/stremio-server-go/internal/types"
)

// relayFetch fetches url through the test client and returns status and body.
func relayFetch(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec // url is our own loopback relay
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func mustAllow(t *testing.T, spec string) *netguard.Allow {
	t.Helper()
	a, bad := netguard.ParseAllow(spec)
	if a == nil || len(bad) > 0 {
		t.Fatalf("ParseAllow(%q) = %v, bad %v", spec, a, bad)
	}
	return a
}

// TestMediaRelayPrivateAllowReachesAllowlistedHost: with private destinations
// blocked, an allowlisted address is reachable for the initial URL, a
// redirect and the nested playlist entries, while the same relay without the
// allowlist still refuses it.
func TestMediaRelayPrivateAllowReachesAllowlistedHost(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/hls/index.m3u8", http.StatusFound)
	})
	mux.HandleFunc("/hls/index.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.0,\nseg0.ts\n#EXT-X-ENDLIST\n")
	})
	mux.HandleFunc("/hls/seg0.ts", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "segment-bytes")
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()

	blocked := newMediaRelay(true)
	u, err := blocked.register(upstream.URL + "/start")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if code, _ := relayFetch(t, u); code != http.StatusBadGateway {
		t.Fatalf("without allowlist: status %d, want %d", code, http.StatusBadGateway)
	}

	r := newMediaRelay(true)
	r.privateAllow.Store(mustAllow(t, "127.0.0.1"))
	u, err = r.register(upstream.URL + "/start")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	code, playlist := relayFetch(t, u)
	if code != http.StatusOK {
		t.Fatalf("with allowlist: status %d, want 200", code)
	}
	var seg string
	for _, line := range strings.Split(playlist, "\n") {
		if strings.HasPrefix(line, "http://127.0.0.1:") {
			seg = line
		}
	}
	if seg == "" {
		t.Fatalf("segment not rewritten to a relay URL:\n%s", playlist)
	}
	if code, body := relayFetch(t, seg); code != http.StatusOK || body != "segment-bytes" {
		t.Fatalf("nested segment: status %d body %q\nplaylist:\n%s", code, body, playlist)
	}
}

// TestMediaRelayPrivateAllowChecksDialedIP: an allowlisted origin cannot
// redirect the relay to a private address outside the list; the check runs
// on the IP actually dialed, not on the first URL.
func TestMediaRelayPrivateAllowChecksDialedIP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skipf("127.0.0.2 not bindable here: %v", err)
	}
	other := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "other-host")
	}))
	_ = other.Listener.Close()
	other.Listener = ln
	other.Start()
	defer other.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, other.URL+"/x", http.StatusFound)
	}))
	defer origin.Close()

	r := newMediaRelay(true)
	r.privateAllow.Store(mustAllow(t, "127.0.0.1"))
	u, err := r.register(origin.URL)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if code, _ := relayFetch(t, u); code != http.StatusBadGateway {
		t.Fatalf("redirect to non-allowlisted 127.0.0.2: status %d, want %d", code, http.StatusBadGateway)
	}

	r.privateAllow.Store(mustAllow(t, "127.0.0.0/30"))
	if code, body := relayFetch(t, u); code != http.StatusOK || body != "other-host" {
		t.Fatalf("with 127.0.0.0/30 allowlisted: status %d body %q", code, body)
	}
}

// TestNewAppliesPrivateAllowToRelay: media.New hands HLSConfig.PrivateAllow
// to the process-wide relay, and a config without it restores the default.
func TestNewAppliesPrivateAllowToRelay(t *testing.T) {
	t.Cleanup(func() { globalRelay.setPrivateAllow(nil) })
	a := mustAllow(t, "10.0.0.0/8")
	cfg := DefaultHLSConfig()
	cfg.PrivateAllow = a
	p := New("http://127.0.0.1:11470", cfg, nil)
	if c, ok := p.(interface{ CloseHLS() }); ok {
		defer c.CloseHLS()
	}
	if globalRelay.privateAllow.Load() != a {
		t.Fatal("New did not apply HLSConfig.PrivateAllow to the relay")
	}
	p2 := New("http://127.0.0.1:11470", DefaultHLSConfig(), nil)
	if c, ok := p2.(interface{ CloseHLS() }); ok {
		defer c.CloseHLS()
	}
	if globalRelay.privateAllow.Load() != nil {
		t.Fatal("New without PrivateAllow did not clear the relay's list")
	}
}

// lanMediaURL is a LAN-style IP literal: no DNS lookup, and never fetched
// because the probe caches below are pre-seeded for it.
const lanMediaURL = "http://192.168.1.10/movie.mkv"

// TestStartHLSPrivateAllow: StartHLSContext refuses a LAN mediaURL by
// default and accepts it once its host is on HLSConfig.PrivateAllow.
func TestStartHLSPrivateAllow(t *testing.T) {
	seed := func(m *hlsManager) {
		m.probeCache[lanMediaURL] = probeCacheEntry{
			result:    probeMediaResult{duration: 60, width: 1920, height: 1080},
			expiresAt: time.Now().Add(time.Hour),
		}
	}

	m := newOverrideTestManager(t, false, DefaultHLSConfig(), nil)
	seed(m)
	if _, err := m.StartHLSContext(context.Background(), "lan", lanMediaURL, types.HLSSessionOptions{}); err == nil {
		t.Fatal("LAN mediaURL accepted without STREMIO_HLS_PRIVATE_ALLOW")
	}

	cfg := DefaultHLSConfig()
	cfg.PrivateAllow = mustAllow(t, "192.168.1.0/24")
	m = newOverrideTestManager(t, false, cfg, nil)
	seed(m)
	master, err := m.StartHLSContext(context.Background(), "lan", lanMediaURL, types.HLSSessionOptions{})
	if err != nil {
		t.Fatalf("allowlisted LAN mediaURL refused: %v", err)
	}
	if !strings.Contains(master, "#EXT-X-STREAM-INF:") {
		t.Errorf("no master playlist for the allowlisted session:\n%s", master)
	}

	// Another private host and cloud-metadata stay refused.
	for _, u := range []string{"http://10.0.0.5/movie.mkv", "http://169.254.169.254/latest/x"} {
		if _, err := m.StartHLSContext(context.Background(), "other", u, types.HLSSessionOptions{}); err == nil {
			t.Errorf("%s accepted with only 192.168.1.0/24 allowlisted", u)
		}
	}
}

// TestProbePrivateAllow: ProbeContext and TracksContext apply the same list
// before anything is fetched, while subtitle and /opensubHash fetches keep
// refusing private hosts.
func TestProbePrivateAllow(t *testing.T) {
	newProber := func(allow *netguard.Allow) *prober {
		p := &prober{
			baseURLLocal: "http://127.0.0.1:11470",
			privateAllow: allow,
			probeCache:   map[string]probeResultEntry{},
			tracksCache:  map[string]tracksCacheEntry{},
		}
		p.probeCache[lanMediaURL] = probeResultEntry{result: map[string]interface{}{"ok": true}, expiresAt: time.Now().Add(time.Hour)}
		p.tracksCache[lanMediaURL] = tracksCacheEntry{result: []interface{}{}, expiresAt: time.Now().Add(time.Hour)}
		return p
	}

	if _, err := newProber(nil).ProbeContext(context.Background(), lanMediaURL); err == nil {
		t.Fatal("ProbeContext accepted a LAN URL without an allowlist")
	}
	p := newProber(mustAllow(t, "192.168.1.10"))
	if _, err := p.ProbeContext(context.Background(), lanMediaURL); err != nil {
		t.Fatalf("ProbeContext refused an allowlisted LAN URL: %v", err)
	}
	if _, err := newProber(nil).TracksContext(context.Background(), lanMediaURL); err == nil {
		t.Fatal("TracksContext accepted a LAN URL without an allowlist")
	}
	if _, err := p.TracksContext(context.Background(), lanMediaURL); err != nil {
		t.Fatalf("TracksContext refused an allowlisted LAN URL: %v", err)
	}
	if _, err := p.OpenSubHashContext(context.Background(), lanMediaURL); err == nil {
		t.Error("OpenSubHash accepted a LAN URL: it must not use the media allowlist")
	}
	if _, err := fetchSubBytes(lanMediaURL, p.baseURLLocal); err == nil {
		t.Error("subtitle fetch accepted a LAN URL: it must not use the media allowlist")
	}
}

// TestValidatePersistedURLPrivateAllow: a persisted session on an
// allowlisted LAN host is restored; without the list it is discarded.
func TestValidatePersistedURLPrivateAllow(t *testing.T) {
	if err := validatePersistedURL(lanMediaURL, "", nil); err == nil {
		t.Error("persisted LAN URL accepted without an allowlist")
	}
	if err := validatePersistedURL(lanMediaURL, "", mustAllow(t, "192.168.1.10")); err != nil {
		t.Errorf("persisted allowlisted LAN URL refused: %v", err)
	}
}

// TestHLSPersistPrivateAllowRestore: a session on an allowlisted LAN host
// starts, is persisted and is restored after a restart with the same
// list; a restart without the list discards it.
func TestHLSPersistPrivateAllowRestore(t *testing.T) {
	work := t.TempDir()
	cfg := HLSConfig{SegmentTimeout: time.Second, PrivateAllow: mustAllow(t, "192.168.1.0/24")}
	fakeProbe := func(context.Context, string, string, time.Duration) probeMediaResult {
		return probeMediaResult{duration: 20, width: 1280, height: 720}
	}

	m1 := newTestPersistManager(t, work, cfg, nil)
	m1.probe = fakeProbe
	if _, err := m1.StartHLS("lan", lanMediaURL, types.HLSSessionOptions{}); err != nil {
		t.Fatalf("StartHLS on an allowlisted LAN host: %v", err)
	}
	m1.CloseHLS()

	m2 := newTestPersistManager(t, work, cfg, nil)
	if n := m2.Sessions(); n != 1 {
		t.Fatalf("Sessions() after restart with the list = %d, want 1", n)
	}
	m2.CloseHLS()

	cfg.PrivateAllow = nil
	m3 := newTestPersistManager(t, work, cfg, nil)
	if n := m3.Sessions(); n != 0 {
		t.Fatalf("Sessions() after restart without the list = %d, want 0", n)
	}
}
