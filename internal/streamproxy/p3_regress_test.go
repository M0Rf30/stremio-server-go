// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package streamproxy

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// externalBase: forwarded headers trust
// ---------------------------------------------------------------------------

func extBaseReq(peer, proto, host string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "internal:8080"
	r.RemoteAddr = peer
	if proto != "" {
		r.Header.Set("X-Forwarded-Proto", proto)
	}
	if host != "" {
		r.Header.Set("X-Forwarded-Host", host)
	}
	return r
}

func TestExternalBasePublicPeerIgnoresForwarded(t *testing.T) {
	h := New(Config{})
	got := h.externalBase(extBaseReq("203.0.113.9:4000", "https", "evil.example"))
	if got != "http://internal:8080" {
		t.Errorf("public peer forwarded headers honoured: %q", got)
	}
}

func TestExternalBasePublicURLWins(t *testing.T) {
	h := New(Config{PublicURL: "https://pub.example/"})
	got := h.externalBase(extBaseReq("127.0.0.1:1", "http", "other.example"))
	if got != "https://pub.example" {
		t.Errorf("got %q", got)
	}
}

func TestExternalBaseForwardedValidation(t *testing.T) {
	h := New(Config{})
	cases := []struct {
		name, proto, host, want string
	}{
		{"first element", "https, http", "a.example, b.example", "https://a.example"},
		{"host with port", "https", "a.example:8443", "https://a.example:8443"},
		{"ipv6", "https", "[2001:db8::1]:443", "https://[2001:db8::1]:443"},
		{"uppercase proto", "HTTPS", "a.example", "https://a.example"},
		{"bad proto", "javascript", "a.example", "http://a.example"},
		{"path in host", "https", "evil.example/x", "https://internal:8080"},
		{"userinfo in host", "https", "user@evil.example", "https://internal:8080"},
		{"space in host", "https", "a b", "https://internal:8080"},
		{"bad port", "https", "a.example:99999", "https://internal:8080"},
		{"empty port", "https", "a.example:", "https://internal:8080"},
		{"quote in host", "https", `a"b.example`, "https://internal:8080"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := h.externalBase(extBaseReq("127.0.0.5:1234", tc.proto, tc.host))
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// clientIP: right-most untrusted hop
// ---------------------------------------------------------------------------

func xffReq(peer string, xff ...string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = peer
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

func TestClientIPRightmostUntrusted(t *testing.T) {
	cases := []struct {
		name string
		r    *http.Request
		want string
	}{
		{"public peer ignores xff", xffReq("203.0.113.9:1", "1.2.3.4"), "203.0.113.9"},
		{"no xff", xffReq("127.0.0.2:1"), "127.0.0.2"},
		{"single hop", xffReq("127.0.0.2:1", "198.51.100.7"), "198.51.100.7"},
		{"spoofed leftmost", xffReq("127.0.0.2:1", "192.168.1.1, 198.51.100.7"), "198.51.100.7"},
		{"spoofed public leftmost", xffReq("127.0.0.2:1", "8.8.8.8, 198.51.100.7"), "198.51.100.7"},
		{"trusted proxies skipped", xffReq("127.0.0.2:1", "198.51.100.7, 127.0.0.9, 127.0.0.1"), "198.51.100.7"},
		{"multiple headers", xffReq("127.0.0.2:1", "8.8.8.8", "198.51.100.7, 127.0.0.9"), "198.51.100.7"},
		{"all private uses leftmost", xffReq("127.0.0.1:1", "127.0.0.5, 127.0.0.9"), "127.0.0.5"},
		{"garbage hop falls back to peer", xffReq("127.0.0.2:1", "198.51.100.7, bogus"), "127.0.0.2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clientIP(tc.r); got == nil || got.String() != tc.want {
				t.Errorf("got %v want %s", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// generate_url validation / token binding
// ---------------------------------------------------------------------------

func generateReq(h *Handler, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/generate_url", strings.NewReader(body))
	r.RemoteAddr = "203.0.113.1:1234"
	h.HandleGenerateURL(w, r)
	return w
}

func TestGenerateURLValidation(t *testing.T) {
	h := New(Config{Secret: testSecret})
	good := base64.RawURLEncoding.EncodeToString([]byte("https://cdn.example/v.ts"))
	cases := []struct {
		name string
		body string
		want int
	}{
		{"ok base64 d", `{"endpoint":"/proxy/stream","expiry_seconds":60,"params":{"d":"` + good + `"}}`, 200},
		{"ok plain d", `{"endpoint":"/proxy/hls/manifest.m3u8","expiry_seconds":60,"params":{"d":"https://cdn.example/a.m3u8"}}`, 200},
		{"ok ip", `{"endpoint":"/proxy/stream","expiry_seconds":60,"ip":"203.0.113.5"}`, 200},
		{"unknown endpoint", `{"endpoint":"/etc/passwd","expiry_seconds":60}`, 400},
		{"empty endpoint", `{"expiry_seconds":60}`, 400},
		{"endpoint with query", `{"endpoint":"/proxy/stream?x=1","expiry_seconds":60}`, 400},
		{"zero expiry", `{"endpoint":"/proxy/stream","expiry_seconds":0}`, 400},
		{"negative expiry", `{"endpoint":"/proxy/stream","expiry_seconds":-5}`, 400},
		{"huge expiry", `{"endpoint":"/proxy/stream","expiry_seconds":9223372036854775807}`, 400},
		{"too long expiry", `{"endpoint":"/proxy/stream","expiry_seconds":99999999}`, 400},
		{"non-http d", `{"endpoint":"/proxy/stream","expiry_seconds":60,"params":{"d":"file:///etc/passwd"}}`, 400},
		{"garbage d", `{"endpoint":"/proxy/stream","expiry_seconds":60,"params":{"d":"%%%"}}`, 400},
		{"base64 non-http d", `{"endpoint":"/proxy/stream","expiry_seconds":60,"params":{"d":"` +
			base64.RawURLEncoding.EncodeToString([]byte("ftp://x/y")) + `"}}`, 400},
		{"bad ip", `{"endpoint":"/proxy/stream","expiry_seconds":60,"ip":"not-an-ip"}`, 400},
		{"oversized body", `{"endpoint":"/proxy/stream","expiry_seconds":60,"pad":"` + strings.Repeat("a", 70<<10) + `"}`, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if w := generateReq(h, tc.body); w.Code != tc.want {
				t.Errorf("status %d want %d: %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestAuthorizeSealedParamExactOnce(t *testing.T) {
	h := New(Config{Secret: testSecret, Password: "pw"})
	d := base64.RawURLEncoding.EncodeToString([]byte("https://cdn.example/v.ts"))
	tok, err := h.signToken(token{
		Endpoint: "/proxy/stream",
		Params:   map[string]string{"d": d},
		Exp:      time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(q string) *http.Request {
		r := httptest.NewRequest("GET", "/proxy/stream?"+q, nil)
		r.RemoteAddr = "203.0.113.1:1"
		return r
	}
	if err := h.authorize(mk("d=" + d + "&token=" + url.QueryEscape(tok))); err != nil {
		t.Errorf("exact d rejected: %v", err)
	}
	other := base64.RawURLEncoding.EncodeToString([]byte("https://evil.example/x"))
	for _, q := range []string{
		"d=" + d + "&d=" + other + "&token=" + url.QueryEscape(tok),
		"d=" + other + "&d=" + d + "&token=" + url.QueryEscape(tok),
		"d=" + other + "&token=" + url.QueryEscape(tok),
		"token=" + url.QueryEscape(tok),
	} {
		if err := h.authorize(mk(q)); err == nil {
			t.Errorf("accepted %q", q)
		}
	}
}

func TestDashTemplateTokenDestPrefix(t *testing.T) {
	h := New(Config{PublicURL: "https://ext.example", Password: "pw", Secret: testSecret})
	opts := &Options{subTokenExp: time.Now().Add(time.Hour).Unix()}
	tmpl := dashBuildTemplateURL(h, "https://ext.example", "https://cdn.example/dash/seg-$Number$.m4s", opts)
	u, err := url.Parse(tmpl)
	if err != nil {
		t.Fatal(err)
	}
	tok := u.Query().Get("token")
	if tok == "" {
		t.Fatalf("no token in %s", tmpl)
	}
	req := func(dest string) *http.Request {
		r := httptest.NewRequest("GET", "/proxy/stream?d="+url.QueryEscape(dest)+"&token="+url.QueryEscape(tok), nil)
		r.RemoteAddr = "203.0.113.1:1"
		return r
	}
	// The player expands the placeholder before requesting.
	if err := h.authorize(req("https://cdn.example/dash/seg-17.m4s")); err != nil {
		t.Errorf("expanded template rejected: %v", err)
	}
	// The unexpanded form as emitted in the MPD also passes.
	if err := h.authorize(req("https://cdn.example/dash/seg-$Number$.m4s")); err != nil {
		t.Errorf("template rejected: %v", err)
	}
	for _, bad := range []string{
		"https://evil.example/dash/seg-17.m4s",
		"https://cdn.example.evil.example/dash/seg-17.m4s",
		"https://cdn.example/other/seg-17.m4s",
	} {
		if err := h.authorize(req(bad)); err == nil {
			t.Errorf("token accepted for %s", bad)
		}
	}

	// A placeholder inside the host cannot pin an origin: endpoint-only binding.
	if p := dashTemplateTokenParams("https://$RepresentationID$.cdn.example/x.m4s"); p != nil {
		t.Errorf("expected nil params, got %v", p)
	}
}

// ---------------------------------------------------------------------------
// HLS tag coverage
// ---------------------------------------------------------------------------

func TestHlsRewriteLLHLSAndSessionTags(t *testing.T) {
	h := hlsNewHandler()
	r := httptest.NewRequest("GET", "/proxy/hls/manifest.m3u8", nil)
	opts := hlsNewOpts()
	playlist := strings.Join([]string{
		`#EXTM3U`,
		`#EXT-X-SESSION-KEY:METHOD=AES-128,URI="sk.bin"`,
		`#EXT-X-MAP:URI="init.mp4"`,
		`#EXT-X-PART:DURATION=0.33,URI="part1.mp4",INDEPENDENT=YES`,
		`#EXT-X-PRELOAD-HINT:TYPE=PART,URI="part2.mp4"`,
		`#EXT-X-RENDITION-REPORT:URI="../audio/live.m3u8",LAST-MSN=3`,
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",URI="audio/main.m3u8"`,
		`#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=1,URI="iframe.m3u8"`,
		`#EXT-X-MEDIA-SEQUENCE:12`,
	}, "\n")
	got := hlsRewrite(h, r, opts, playlist)
	lines := strings.Split(got, "\n")
	byTag := map[string]string{}
	for _, l := range lines {
		tag, _, _ := strings.Cut(l, ":")
		byTag[tag] = l
	}
	for _, tag := range []string{"#EXT-X-SESSION-KEY", "#EXT-X-MAP", "#EXT-X-PART", "#EXT-X-PRELOAD-HINT"} {
		if !strings.Contains(byTag[tag], `URI="https://ext.example/proxy/stream?d=`) {
			t.Errorf("%s not proxied via stream: %s", tag, byTag[tag])
		}
	}
	for _, tag := range []string{"#EXT-X-RENDITION-REPORT", "#EXT-X-MEDIA", "#EXT-X-I-FRAME-STREAM-INF"} {
		if !strings.Contains(byTag[tag], `URI="https://ext.example/proxy/hls/manifest.m3u8?d=`) {
			t.Errorf("%s not proxied via hls: %s", tag, byTag[tag])
		}
	}
	if !strings.Contains(byTag["#EXT-X-PART"], ",INDEPENDENT=YES") || !strings.Contains(byTag["#EXT-X-PART"], "DURATION=0.33,") {
		t.Errorf("PART attrs lost: %s", byTag["#EXT-X-PART"])
	}
	if byTag["#EXT-X-MEDIA-SEQUENCE"] != "#EXT-X-MEDIA-SEQUENCE:12" {
		t.Errorf("MEDIA-SEQUENCE altered: %s", byTag["#EXT-X-MEDIA-SEQUENCE"])
	}
	// Relative URIs must resolve against the playlist URL.
	enc := base64.RawURLEncoding.EncodeToString([]byte("https://cdn.example/live/part1.mp4"))
	if !strings.Contains(byTag["#EXT-X-PART"], "d="+enc) {
		t.Errorf("PART not resolved against base: %s", byTag["#EXT-X-PART"])
	}
	encRR := base64.RawURLEncoding.EncodeToString([]byte("https://cdn.example/audio/live.m3u8"))
	if !strings.Contains(byTag["#EXT-X-RENDITION-REPORT"], "d="+encRR) {
		t.Errorf("RENDITION-REPORT not resolved: %s", byTag["#EXT-X-RENDITION-REPORT"])
	}
}

func TestHlsRewriteLeavesNonHTTPURIs(t *testing.T) {
	h := hlsNewHandler()
	r := httptest.NewRequest("GET", "/proxy/hls/manifest.m3u8", nil)
	opts := hlsNewOpts()
	playlist := strings.Join([]string{
		`#EXT-X-KEY:METHOD=SAMPLE-AES,URI="skd://key-id",KEYFORMAT="com.apple.streamingkeydelivery"`,
		`#EXT-X-KEY:METHOD=SAMPLE-AES,URI="data:text/plain;base64,AAAA",KEYFORMAT="urn:uuid:edef8ba9"`,
		`#EXT-X-SESSION-KEY:METHOD=AES-128,URI=""`,
		`#EXT-X-MAP:URI="data:video/mp4;base64,AAAA"`,
		`data:text/plain,hello`,
		`seg.ts`,
	}, "\n")
	got := hlsRewrite(h, r, opts, playlist)
	lines := strings.Split(got, "\n")
	for i := range 5 {
		if lines[i] != strings.Split(playlist, "\n")[i] {
			t.Errorf("line %d changed: %q", i, lines[i])
		}
	}
	if !strings.HasPrefix(lines[5], "https://ext.example/proxy/stream?d=") {
		t.Errorf("segment not proxied: %q", lines[5])
	}

	urls := hlsSegmentURLs("https://cdn.example/live/index.m3u8", playlist, 10)
	if len(urls) != 1 || urls[0] != "https://cdn.example/live/seg.ts" {
		t.Errorf("prefetch urls = %v", urls)
	}
}

// ---------------------------------------------------------------------------
// parseOptions '+' handling
// ---------------------------------------------------------------------------

func TestParseOptionsPreservesPlus(t *testing.T) {
	h := New(Config{})
	dest := "https://cdn.example/a+b/seg.ts?sig=x+y/z"
	r := httptest.NewRequest("GET", "/?d="+url.QueryEscape(dest), nil)
	opts, err := h.parseOptions(r)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Dest != dest {
		t.Errorf("Dest = %q want %q", opts.Dest, dest)
	}
}

func TestParseOptionsStdBase64WithPlus(t *testing.T) {
	h := New(Config{})
	dest := "https://cdn.example/>>>>?q=~~"
	enc := base64.StdEncoding.EncodeToString([]byte(dest))
	if !strings.Contains(enc, "+") {
		t.Fatalf("test input has no '+' in base64: %s", enc)
	}
	// Unescaped '+' reaches the handler as a space after query parsing.
	for name, q := range map[string]string{"raw": enc, "escaped": url.QueryEscape(enc)} {
		r := httptest.NewRequest("GET", "/?d="+q, nil)
		opts, err := h.parseOptions(r)
		if err != nil {
			t.Fatal(err)
		}
		if opts.Dest != dest {
			t.Errorf("%s: Dest = %q want %q", name, opts.Dest, dest)
		}
	}
}

// ---------------------------------------------------------------------------
// Segment cache: janitor + single-flight
// ---------------------------------------------------------------------------

func TestSegCacheJanitorSweepsExpired(t *testing.T) {
	c := newSegCache(20*time.Millisecond, 10)
	c.sweepEvery = 5 * time.Millisecond
	c.putFull("a", []byte("aaa"), nil, 200)
	c.putFull("b", []byte("bb"), nil, 200)

	deadline := time.Now().Add(3 * time.Second)
	for {
		c.mu.Lock()
		n, bytes := len(c.items), c.totalBytes
		c.mu.Unlock()
		if n == 0 && bytes == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("janitor did not sweep: %d entries, %d bytes", n, bytes)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSegCacheSweepKeepsFresh(t *testing.T) {
	c := newSegCache(time.Hour, 10)
	c.putFull("a", []byte("aaa"), nil, 200)
	c.mu.Lock()
	c.items["a"].Value.(*cacheEntry).expiresAt = time.Now().Add(-time.Second)
	c.mu.Unlock()
	c.putFull("b", []byte("bb"), nil, 200)
	c.sweep()
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items["a"]; ok {
		t.Error("expired entry survived sweep")
	}
	if _, ok := c.items["b"]; !ok {
		t.Error("fresh entry swept")
	}
	if c.totalBytes != 2 || c.lru.Len() != 1 {
		t.Errorf("bookkeeping off: bytes=%d len=%d", c.totalBytes, c.lru.Len())
	}
}

func TestCachedFetchSingleFlight(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release
		_, _ = w.Write([]byte("segment-body"))
	}))
	defer srv.Close()

	h := New(Config{SegCacheTTL: time.Minute, Client: srv.Client()})
	const n = 6
	var wg sync.WaitGroup
	results := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, _, _, err := h.cachedFetch(t.Context(), srv.URL+"/seg.ts", nil, "")
			results[i], errs[i] = string(b), err
		}()
	}
	// Wait for the leader to reach the upstream, then let followers pile up.
	deadline := time.Now().Add(3 * time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := hits.Load(); got != 1 {
		t.Errorf("upstream hits = %d, want 1", got)
	}
	for i := range n {
		if errs[i] != nil || results[i] != "segment-body" {
			t.Errorf("caller %d: body=%q err=%v", i, results[i], errs[i])
		}
	}
	if len(h.flights) != 0 {
		t.Errorf("flights leaked: %d", len(h.flights))
	}
}

func TestCachedFetchFollowerRetriesAfterLeaderError(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	h := New(Config{SegCacheTTL: time.Minute, Client: srv.Client()})

	key := cacheKey(srv.URL+"/s.ts", nil)
	call, leader := h.flightJoin(key)
	if !leader {
		t.Fatal("expected to lead")
	}
	done := make(chan struct{})
	var body []byte
	var err error
	go func() {
		defer close(done)
		body, _, _, err = h.cachedFetch(t.Context(), srv.URL+"/s.ts", nil, "")
	}()
	time.Sleep(20 * time.Millisecond)
	call.err = http.ErrAbortHandler // leader failed
	h.flightFinish(key, call)
	<-done
	if err != nil || string(body) != "ok" {
		t.Errorf("follower did not recover: body=%q err=%v", body, err)
	}
}

func TestServeStreamWaitsForInflightPrefetch(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release
		_, _ = w.Write([]byte("seg"))
	}))
	defer srv.Close()
	h := New(Config{SegCacheTTL: time.Minute, Prebuffer: 1, Client: srv.Client(), PublicURL: "https://ext.example"})

	seg := srv.URL + "/s.ts"
	h.prefetch(t.Context(), []string{seg}, nil, "")
	deadline := time.Now().Add(3 * time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest("GET", "/proxy/stream?d="+url.QueryEscape(seg), nil)
		h.serveStream(w, req)
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	<-done
	if w.Code != 200 || w.Body.String() != "seg" {
		t.Errorf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("upstream hits = %d, want 1 (client should reuse prefetch)", got)
	}
}
