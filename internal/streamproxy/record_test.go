// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package streamproxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func recReq(host, query string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/record?"+query, nil)
	r.Host = host
	return r
}

func splitProxyPath(t *testing.T, p string) (string, url.Values) {
	t.Helper()
	u, err := url.Parse(p)
	if err != nil {
		t.Fatalf("bad path %q: %v", p, err)
	}
	return u.Path, u.Query()
}

func recStatus(t *testing.T, err error) int {
	t.Helper()
	var re *RecordError
	if !errors.As(err, &re) {
		t.Fatalf("want *RecordError, got %v", err)
	}
	return re.Status
}

func TestResolveRecordSourceByExtension(t *testing.T) {
	h := New(Config{AppPath: t.TempDir()})
	t.Cleanup(h.Close)
	cases := []struct{ src, endpoint string }{
		{"http://93.184.216.34/live/index.m3u8?tok=1", "/proxy/hls/manifest.m3u8"},
		{"http://93.184.216.34/playlist.m3u", "/proxy/hls/manifest.m3u8"},
		{"http://93.184.216.34/dash/manifest.mpd", "/proxy/mpd/manifest.m3u8"},
		{"http://93.184.216.34/movie.mp4", "/proxy/stream"},
		{"http://93.184.216.34/live.ts", "/proxy/stream"},
	}
	for _, c := range cases {
		p, err := h.ResolveRecordSource(recReq("srv", ""), RecordSource{URL: c.src})
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
		path, q := splitProxyPath(t, p)
		if path != c.endpoint {
			t.Errorf("%s -> %s; want %s", c.src, path, c.endpoint)
		}
		if got := decodeDest(q.Get("d")); got != c.src {
			t.Errorf("%s: d = %q", c.src, got)
		}
		if q.Has("api_password") {
			t.Errorf("%s: api_password set with no password configured", c.src)
		}
	}
}

func TestResolveRecordSourceAttachesPasswordAndHeaders(t *testing.T) {
	h := New(Config{AppPath: t.TempDir(), Password: "pw"})
	t.Cleanup(h.Close)
	r := recReq("srv", "h_Referer=https%3A%2F%2Fref.example%2F&h_Host=evil&h_X-Token=abc")
	p, err := h.ResolveRecordSource(r, RecordSource{URL: "http://93.184.216.34/a.m3u8"})
	if err != nil {
		t.Fatal(err)
	}
	_, q := splitProxyPath(t, p)
	if q.Get("api_password") != "pw" {
		t.Errorf("api_password = %q", q.Get("api_password"))
	}
	if q.Get("h_Referer") != "https://ref.example/" || q.Get("h_X-Token") != "abc" {
		t.Errorf("headers not forwarded: %v", q)
	}
	if q.Has("h_Host") {
		t.Error("blocked request header h_Host was forwarded")
	}
}

func TestResolveRecordSourceExtractor(t *testing.T) {
	up := newEmbedUpstream(t)
	h := newEmbedHandler(t)

	// Auto-matched embed page -> its media URL on the endpoint the definition names.
	p, err := h.ResolveRecordSource(recReq("srv", ""), RecordSource{URL: up.srv.URL + "/e/abc123"})
	if err != nil {
		t.Fatal(err)
	}
	path, q := splitProxyPath(t, p)
	if path != "/proxy/stream" || decodeDest(q.Get("d")) != up.srv.URL+"/media/v.mp4" {
		t.Errorf("embed page resolved to %s %s", path, decodeDest(q.Get("d")))
	}
	if q.Get("h_X-From-Def") != "def" || q.Get("h_Referer") != up.srv.URL+"/e/abc123" {
		t.Errorf("definition headers lost: %v", q)
	}

	// HLS embed picks the playlist endpoint.
	p, err = h.ResolveRecordSource(recReq("srv", ""), RecordSource{URL: up.srv.URL + "/hls-embed/9"})
	if err != nil {
		t.Fatal(err)
	}
	if path, _ := splitProxyPath(t, p); path != "/proxy/hls/manifest.m3u8" {
		t.Errorf("hls embed endpoint = %s", path)
	}

	// Forced extractor on a page the patterns do not match.
	p, err = h.ResolveRecordSource(recReq("srv", ""), RecordSource{URL: up.srv.URL + "/hls-embed/9", Host: "embedhost"})
	if err == nil {
		t.Fatalf("forced embedhost on a page without the expected script succeeded: %s", p)
	}
	if got := recStatus(t, err); got != http.StatusBadGateway {
		t.Errorf("extraction failure status = %d; want 502", got)
	}

	// Unknown forced extractor.
	_, err = h.ResolveRecordSource(recReq("srv", ""), RecordSource{URL: up.srv.URL + "/e/1", Host: "nope"})
	if got := recStatus(t, err); got != http.StatusBadRequest {
		t.Errorf("unknown host status = %d", got)
	}
}

func TestResolveRecordSourceSniffsAmbiguousURLs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/hls", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("#EXTM3U\n#EXT-X-VERSION:3\n")) })
	mux.HandleFunc("/dash", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0"?><MPD type="dynamic"></MPD>`))
	})
	mux.HandleFunc("/ts", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "" {
			t.Error("sniff request carried no Range")
		}
		_, _ = w.Write([]byte{0x47, 0x40, 0x00, 0x10, 0, 0, 0})
	})
	mux.HandleFunc("/down", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusForbidden) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	h := New(Config{AppPath: t.TempDir()})
	t.Cleanup(h.Close)
	for path, want := range map[string]string{
		"/hls":  "/proxy/hls/manifest.m3u8",
		"/dash": "/proxy/mpd/manifest.m3u8",
		"/ts":   "/proxy/stream",
		"/down": "/proxy/hls/manifest.m3u8", // unreadable -> EasyProxy default
	} {
		p, err := h.ResolveRecordSource(recReq("srv", ""), RecordSource{URL: srv.URL + path})
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if got, _ := splitProxyPath(t, p); got != want {
			t.Errorf("%s -> %s; want %s", path, got, want)
		}
	}
}

func TestResolveRecordSourceSelfProxyURL(t *testing.T) {
	h := New(Config{AppPath: t.TempDir(), Password: "pw", PublicURL: "https://ext.example"})
	t.Cleanup(h.Close)

	// Our own /proxy URL is used as-is; the server password is added if absent.
	own := "https://ext.example/proxy/hls/manifest.m3u8?d=aHR0cDovL3gvYS5tM3U4&h_Referer=r"
	p, err := h.ResolveRecordSource(recReq("ext.example", ""), RecordSource{URL: own})
	if err != nil {
		t.Fatal(err)
	}
	path, q := splitProxyPath(t, p)
	if path != "/proxy/hls/manifest.m3u8" || q.Get("d") != "aHR0cDovL3gvYS5tM3U4" || q.Get("h_Referer") != "r" || q.Get("api_password") != "pw" {
		t.Errorf("self URL = %s", p)
	}

	// An existing credential is kept, not overwritten.
	p, err = h.ResolveRecordSource(recReq("ext.example", ""), RecordSource{URL: own + "&api_password=given"})
	if err != nil {
		t.Fatal(err)
	}
	if _, q := splitProxyPath(t, p); q.Get("api_password") != "given" {
		t.Errorf("existing api_password replaced: %s", p)
	}

	// Same path on a foreign host is NOT trusted as ours: it gets wrapped.
	p, err = h.ResolveRecordSource(recReq("ext.example", ""), RecordSource{URL: "http://93.184.216.34/proxy/hls/manifest.m3u8?d=x"})
	if err != nil {
		t.Fatal(err)
	}
	path, q = splitProxyPath(t, p)
	if path != "/proxy/hls/manifest.m3u8" || decodeDest(q.Get("d")) != "http://93.184.216.34/proxy/hls/manifest.m3u8?d=x" {
		t.Errorf("foreign proxy URL not wrapped: %s", p)
	}
}

func TestResolveRecordSourceRejects(t *testing.T) {
	h := New(Config{AppPath: t.TempDir(), Password: "pw"}) // password => private destinations blocked
	t.Cleanup(h.Close)
	cases := []struct {
		name string
		src  RecordSource
		code int
	}{
		{"empty", RecordSource{}, http.StatusBadRequest},
		{"file scheme", RecordSource{URL: "file:///etc/passwd"}, http.StatusBadRequest},
		{"ftp scheme", RecordSource{URL: "ftp://93.184.216.34/x.ts"}, http.StatusBadRequest},
		{"loopback", RecordSource{URL: "http://127.0.0.1:9/x.m3u8"}, http.StatusBadRequest},
		{"metadata", RecordSource{URL: "http://169.254.169.254/latest/meta-data"}, http.StatusBadRequest},
		{"private lan", RecordSource{URL: "http://192.168.1.10/live.m3u8"}, http.StatusBadRequest},
		{"unknown extractor", RecordSource{URL: "http://93.184.216.34/x", Host: "nope"}, http.StatusBadRequest},
		{"bad proxy scheme", RecordSource{URL: "http://93.184.216.34/x.m3u8", Proxy: "ftp://p:1"}, http.StatusBadRequest},
		{"private proxy", RecordSource{URL: "http://93.184.216.34/x.m3u8", Proxy: "socks5://127.0.0.1:9050"}, http.StatusForbidden},
	}
	for _, c := range cases {
		_, err := h.ResolveRecordSource(recReq("srv", ""), c.src)
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		if got := recStatus(t, err); got != c.code {
			t.Errorf("%s: status %d; want %d (%v)", c.name, got, c.code, err)
		}
	}
	// "off"/"on" are EasyProxy routing keywords, not proxy URLs.
	if _, err := h.ResolveRecordSource(recReq("srv", ""), RecordSource{URL: "http://93.184.216.34/x.m3u8", Proxy: "off"}); err != nil {
		t.Errorf("proxy=off rejected: %v", err)
	}
}

func TestResolveRecordSourceUpstreamProxy(t *testing.T) {
	h := New(Config{AppPath: t.TempDir()})
	t.Cleanup(h.Close)
	p, err := h.ResolveRecordSource(recReq("srv", ""), RecordSource{URL: "http://93.184.216.34/x.m3u8", Proxy: "socks5://93.184.216.35:1080"})
	if err != nil {
		t.Fatal(err)
	}
	if _, q := splitProxyPath(t, p); q.Get("proxy") != "socks5://93.184.216.35:1080" {
		t.Errorf("per-request proxy not carried to the proxy URL: %s", p)
	}
}

func TestIsForbidden(t *testing.T) {
	h := New(Config{AppPath: t.TempDir(), Password: "pw"})
	t.Cleanup(h.Close)
	r := httptest.NewRequest(http.MethodGet, "/record", nil)
	if err := h.Authorize(r); err == nil || IsForbidden(err) {
		t.Errorf("missing password: %v", err)
	}
	if !strings.Contains(recErr(400, "x").Error(), "x") {
		t.Error("RecordError message")
	}
}
