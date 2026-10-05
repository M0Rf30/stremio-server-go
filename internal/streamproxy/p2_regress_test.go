// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package streamproxy

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// decodeProxyD extracts and base64-decodes the d= parameter of a proxy URL.
func decodeProxyD(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	b, err := base64.RawURLEncoding.DecodeString(u.Query().Get("d"))
	if err != nil {
		t.Fatalf("decode d of %q: %v", raw, err)
	}
	return string(b)
}

func TestHlsRewriteCRLFPlaylist(t *testing.T) {
	h := hlsNewHandler()
	r := httptest.NewRequest("GET", "/proxy/hls/manifest.m3u8", nil)
	opts := hlsNewOpts()

	playlist := "#EXTM3U\r\n#EXT-X-TARGETDURATION:6\r\n#EXTINF:6.0,\r\nseg001.ts\r\n\r\n#EXTINF:6.0,\r\nseg002.ts\r\n#EXT-X-ENDLIST\r\n"
	got := hlsRewrite(h, r, opts, playlist)

	if strings.Contains(got, "\r") {
		t.Fatalf("output still contains CR:\n%q", got)
	}
	var uris []string
	for _, l := range strings.Split(got, "\n") {
		if l != "" && !strings.HasPrefix(l, "#") {
			uris = append(uris, l)
		}
	}
	if len(uris) != 2 {
		t.Fatalf("want 2 proxied URIs (blank CRLF line must not become one), got %d: %q", len(uris), uris)
	}
	want := []string{"https://cdn.example/live/seg001.ts", "https://cdn.example/live/seg002.ts"}
	for i, u := range uris {
		if d := decodeProxyD(t, u); d != want[i] {
			t.Errorf("uri %d: dest %q want %q", i, d, want[i])
		}
	}

	// Prefetch URLs must equal the rewritten destinations so cache keys match.
	seg := hlsSegmentURLs(opts.Dest, playlist, 10)
	if len(seg) != 2 || seg[0] != want[0] || seg[1] != want[1] {
		t.Errorf("hlsSegmentURLs = %q want %q", seg, want)
	}
}

func TestHlsRewriteCRLFKeyAndVariant(t *testing.T) {
	h := hlsNewHandler()
	r := httptest.NewRequest("GET", "/proxy/hls/manifest.m3u8", nil)
	opts := hlsNewOpts()
	playlist := "#EXTM3U\r\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\r\n#EXT-X-STREAM-INF:BANDWIDTH=1\r\nv.m3u8\r\n"
	got := hlsRewrite(h, r, opts, playlist)
	if strings.Contains(got, "\r") {
		t.Fatalf("CR leaked:\n%q", got)
	}
	if !strings.Contains(got, "https://ext.example/proxy/hls/manifest.m3u8?d=") {
		t.Errorf("variant not routed to hls endpoint:\n%s", got)
	}
	if !strings.Contains(got, `URI="https://ext.example/proxy/stream?d=`) {
		t.Errorf("key URI not proxied:\n%s", got)
	}
}

func TestHlsServeUpstreamNon2xx(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "<html>oops\nseg.ts</html>", code)
		}))
		h := New(Config{PublicURL: "https://ext.example", Client: srv.Client()})
		req := httptest.NewRequest("GET", "/proxy/hls/manifest.m3u8?d="+url.QueryEscape(srv.URL+"/x.m3u8"), nil)
		w := httptest.NewRecorder()
		hlsServe(h, w, req)
		srv.Close()
		if w.Code != http.StatusBadGateway {
			t.Errorf("upstream %d: got status %d want 502", code, w.Code)
		}
		if strings.Contains(w.Body.String(), "/proxy/stream") {
			t.Errorf("upstream %d: error body was rewritten into a playlist: %s", code, w.Body.String())
		}
	}
}

func TestHlsServeSecurityHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "#EXTM3U\n#EXTINF:1,\nseg.ts\n")
	}))
	defer srv.Close()
	h := New(Config{PublicURL: "https://ext.example", Client: srv.Client()})
	req := httptest.NewRequest("GET", "/proxy/hls/manifest.m3u8?d="+url.QueryEscape(srv.URL+"/x.m3u8")+"&r_X-Content-Type-Options=sniff", nil)
	w := httptest.NewRecorder()
	hlsServe(h, w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	if got := w.Header().Get("Content-Security-Policy"); got != "sandbox" {
		t.Errorf("Content-Security-Policy = %q", got)
	}
}

func TestHlsRewriteTokenAuthMintsSubTokens(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	h := New(Config{PublicURL: "https://ext.example", Password: "pw", Secret: secret})

	dest := "https://cdn.example/live/index.m3u8"
	dB64 := base64.RawURLEncoding.EncodeToString([]byte(dest))
	exp := time.Now().Add(time.Hour).Unix()
	tok, err := h.signToken(token{
		Endpoint: "/proxy/hls/manifest.m3u8",
		Params:   map[string]string{"d": dB64},
		Exp:      exp,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/proxy/hls/manifest.m3u8?d="+dB64+"&token="+url.QueryEscape(tok), nil)
	req.RemoteAddr = "203.0.113.9:1234"
	if err := h.authorize(req); err != nil {
		t.Fatalf("authorize token request: %v", err)
	}
	opts, err := h.parseOptions(req)
	if err != nil {
		t.Fatal(err)
	}
	got := hlsRewrite(h, req, opts, "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"k.bin\"\n#EXTINF:1,\nseg1.ts\n#EXT-X-STREAM-INF:BANDWIDTH=1\nv.m3u8\n")

	var subs []string
	for _, l := range strings.Split(got, "\n") {
		switch {
		case strings.HasPrefix(l, "https://"):
			subs = append(subs, l)
		case strings.Contains(l, `URI="`):
			s := l[strings.Index(l, `URI="`)+5:]
			subs = append(subs, s[:strings.Index(s, `"`)])
		}
	}
	if len(subs) != 3 {
		t.Fatalf("want 3 sub-URLs, got %d:\n%s", len(subs), got)
	}
	for _, s := range subs {
		u, err := url.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		if u.Query().Get("token") == "" {
			t.Fatalf("sub-URL carries no credential: %s", s)
		}
		if u.Query().Get("api_password") != "" {
			t.Fatalf("password must not leak into token-authed URLs: %s", s)
		}
		sub := httptest.NewRequest("GET", u.RequestURI(), nil)
		sub.RemoteAddr = req.RemoteAddr
		if err := h.authorize(sub); err != nil {
			t.Errorf("sub-URL %s rejected: %v", s, err)
		}
		// The token is bound to its destination: swapping d must fail.
		q := u.Query()
		q.Set("d", base64.RawURLEncoding.EncodeToString([]byte("https://evil.example/x")))
		u.RawQuery = q.Encode()
		bad := httptest.NewRequest("GET", u.RequestURI(), nil)
		bad.RemoteAddr = req.RemoteAddr
		if err := h.authorize(bad); err == nil {
			t.Errorf("tampered d accepted for %s", s)
		}
	}
	// Tokens inherit the parent expiry.
	u, _ := url.Parse(subs[0])
	st, err := h.verifyToken(u.Query().Get("token"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Exp != exp {
		t.Errorf("sub-token exp %d want %d", st.Exp, exp)
	}
}

func TestBuildProxyURLNoTokenWithoutPassword(t *testing.T) {
	h := New(Config{PublicURL: "https://ext.example", Secret: []byte("0123456789abcdef0123456789abcdef")})
	dB64 := base64.RawURLEncoding.EncodeToString([]byte("https://cdn.example/a.m3u8"))
	tok, _ := h.signToken(token{Exp: time.Now().Add(time.Hour).Unix()})
	req := httptest.NewRequest("GET", "/proxy/hls/manifest.m3u8?d="+dB64+"&token="+url.QueryEscape(tok), nil)
	opts, _ := h.parseOptions(req)
	if got := h.buildProxyURL("https://ext.example", "/proxy/stream", "https://cdn.example/s.ts", opts); strings.Contains(got, "token=") {
		t.Errorf("unexpected token when no password configured: %s", got)
	}
}

// countingWriter is a ResponseWriter that discards the body and counts bytes.
type countingWriter struct {
	hdr    http.Header
	status int
	n      int64
}

func (c *countingWriter) Header() http.Header { return c.hdr }
func (c *countingWriter) WriteHeader(s int)   { c.status = s }
func (c *countingWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	c.n += int64(len(p))
	return len(p), nil
}

func TestServeStreamLargeNonRangeStreamsWithCache(t *testing.T) {
	size := maxSegmentBytes + 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		_, _ = io.CopyN(w, zeroReader{}, size)
	}))
	defer srv.Close()
	h := New(Config{SegCacheTTL: time.Minute, Client: srv.Client(), PublicURL: "https://ext.example"})

	req := httptest.NewRequest("GET", "/proxy/stream?d="+url.QueryEscape(srv.URL+"/big.bin"), nil)
	w := &countingWriter{hdr: make(http.Header)}
	h.serveStream(w, req)
	if w.status != http.StatusOK {
		t.Fatalf("status %d want 200", w.status)
	}
	if w.n != size {
		t.Fatalf("streamed %d bytes want %d", w.n, size)
	}
	if entries, _ := h.CacheStats(); entries != 0 {
		t.Errorf("large body must not be cached, entries=%d", entries)
	}
}

func TestServeStreamSmallNonRangeCachedWithHeaders(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Content-Length", "5")
		_, _ = io.WriteString(w, "hello")
	}))
	defer srv.Close()
	h := New(Config{SegCacheTTL: time.Minute, Client: srv.Client(), PublicURL: "https://ext.example"})

	for i := range 2 {
		req := httptest.NewRequest("GET", "/proxy/stream?d="+url.QueryEscape(srv.URL+"/s.ts")+"&r_Content-Security-Policy=default-src+*", nil)
		w := httptest.NewRecorder()
		h.serveStream(w, req)
		if w.Code != http.StatusOK || w.Body.String() != "hello" {
			t.Fatalf("req %d: %d %q", i, w.Code, w.Body.String())
		}
		if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("req %d: nosniff = %q", i, got)
		}
		if got := w.Header().Get("Content-Security-Policy"); got != "sandbox" {
			t.Errorf("req %d: CSP = %q", i, got)
		}
	}
	if calls != 1 {
		t.Errorf("upstream calls = %d want 1 (second served from cache)", calls)
	}
}

func TestServeStreamChunkedNonRangeStreamsUncached(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush() // force chunked encoding: no Content-Length
		_, _ = io.WriteString(w, "chunked-body")
	}))
	defer srv.Close()
	h := New(Config{SegCacheTTL: time.Minute, Client: srv.Client(), PublicURL: "https://ext.example"})
	req := httptest.NewRequest("GET", "/proxy/stream?d="+url.QueryEscape(srv.URL+"/c"), nil)
	w := httptest.NewRecorder()
	h.serveStream(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "chunked-body" {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
	if entries, _ := h.CacheStats(); entries != 0 {
		t.Errorf("unknown-length body must not be cached, entries=%d", entries)
	}
}
