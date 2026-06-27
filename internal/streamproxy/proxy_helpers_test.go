package streamproxy

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// isPrivate
// ---------------------------------------------------------------------------

func TestIsPrivate(t *testing.T) {
	cases := []struct {
		ip      string
		private bool
	}{
		{"127.0.0.1", true},
		{"10.0.0.1", true},
		{"10.255.255.255", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"192.168.1.1", true},
		{"169.254.1.1", true},
		{"::1", true},
		{"203.0.113.1", false},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"172.32.0.1", false}, // just outside RFC 1918 /12
	}
	for _, tc := range cases {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("cannot parse IP %q", tc.ip)
		}
		got := isPrivate(ip)
		if got != tc.private {
			t.Errorf("isPrivate(%q) = %v, want %v", tc.ip, got, tc.private)
		}
	}
}

func TestIsPrivateIPv4MappedIPv6(t *testing.T) {
	// ::ffff:10.0.0.1 is an IPv4-mapped IPv6 address that should match the
	// 10.0.0.0/8 range after normalisation.
	ip := net.ParseIP("::ffff:10.0.0.1")
	if !isPrivate(ip) {
		t.Error("expected ::ffff:10.0.0.1 to be private (IPv4-mapped)")
	}
}

// ---------------------------------------------------------------------------
// isCloudMetadata
// ---------------------------------------------------------------------------

func TestIsCloudMetadata(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"169.254.169.254", true},
		{"169.254.1.1", false},
		{"1.2.3.4", false},
		{"::1", false},
	}
	for _, tc := range cases {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("cannot parse %q", tc.ip)
		}
		if got := isCloudMetadata(ip); got != tc.want {
			t.Errorf("isCloudMetadata(%q) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// ValidateDest
// ---------------------------------------------------------------------------

func TestValidateDestSchemeRejected(t *testing.T) {
	h := New(Config{})
	if err := h.ValidateDest("ftp://example.com/file"); err == nil {
		t.Error("expected error for ftp scheme, got nil")
	}
	if err := h.ValidateDest("file:///etc/passwd"); err == nil {
		t.Error("expected error for file scheme, got nil")
	}
}

func TestValidateDestCloudMetadataAlwaysBlocked(t *testing.T) {
	// 169.254.169.254 must be blocked even without password/IPACL.
	h := New(Config{})
	if err := h.ValidateDest("http://169.254.169.254/metadata"); err == nil {
		t.Error("expected error for cloud-metadata IP, got nil")
	}
}

func TestValidateDestPrivateAllowedWhenUnprotected(t *testing.T) {
	// Private IPs are allowed when no password/IPACL is configured.
	h := New(Config{})
	if err := h.ValidateDest("http://10.0.0.1/stream"); err != nil {
		t.Errorf("private IP on unprotected handler should be allowed: %v", err)
	}
}

func TestValidateDestPrivateBlockedWhenProtected(t *testing.T) {
	// Private IPs are blocked when a password is set (handler is "protected").
	h := New(Config{Password: "s"})
	if err := h.ValidateDest("http://10.0.0.1/stream"); err == nil {
		t.Error("expected error for private IP on protected handler, got nil")
	}
}

func TestValidateDestPublicAllowed(t *testing.T) {
	h := New(Config{Password: "s"})
	if err := h.ValidateDest("http://203.0.113.5/stream"); err != nil {
		t.Errorf("public IP should be allowed: %v", err)
	}
}

func TestValidateDestInvalidURL(t *testing.T) {
	h := New(Config{})
	// A completely unparseable URL.
	if err := h.ValidateDest("://bad"); err == nil {
		t.Error("expected error for unparseable URL, got nil")
	}
}

// ---------------------------------------------------------------------------
// tryBase64Decode
// ---------------------------------------------------------------------------

func TestTryBase64DecodeRawURL(t *testing.T) {
	input := "https://cdn.example/seg.ts"
	enc := base64.RawURLEncoding.EncodeToString([]byte(input))
	got, err := tryBase64Decode(enc)
	if err != nil {
		t.Fatalf("tryBase64Decode: %v", err)
	}
	if got != input {
		t.Errorf("decoded: got %q want %q", got, input)
	}
}

func TestTryBase64DecodeStd(t *testing.T) {
	input := "https://cdn.example/seg.ts"
	enc := base64.StdEncoding.EncodeToString([]byte(input))
	got, err := tryBase64Decode(enc)
	if err != nil {
		t.Fatalf("tryBase64Decode (std): %v", err)
	}
	if got != input {
		t.Errorf("decoded: got %q want %q", got, input)
	}
}

func TestTryBase64DecodeInvalid(t *testing.T) {
	_, err := tryBase64Decode("!!!not_base64!!!")
	if err == nil {
		t.Error("expected error for invalid base64, got nil")
	}
}

// ---------------------------------------------------------------------------
// writeAuthError
// ---------------------------------------------------------------------------

func TestWriteAuthErrorForbidden(t *testing.T) {
	w := httptest.NewRecorder()
	writeAuthError(w, errForbidden)
	if w.Code != http.StatusForbidden {
		t.Errorf("status: got %d want 403", w.Code)
	}
}

func TestWriteAuthErrorUnauthorized(t *testing.T) {
	w := httptest.NewRecorder()
	writeAuthError(w, errUnauthorized)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d want 401", w.Code)
	}
}

// ---------------------------------------------------------------------------
// externalBase
// ---------------------------------------------------------------------------

func TestExternalBasePublicURL(t *testing.T) {
	h := New(Config{PublicURL: "https://proxy.example"})
	r := httptest.NewRequest("GET", "/", nil)
	got := h.externalBase(r)
	if got != "https://proxy.example" {
		t.Errorf("got %q want %q", got, "https://proxy.example")
	}
}

func TestExternalBasePublicURLTrailingSlash(t *testing.T) {
	h := New(Config{PublicURL: "https://proxy.example/"})
	r := httptest.NewRequest("GET", "/", nil)
	got := h.externalBase(r)
	// Trailing slash is stripped.
	if got != "https://proxy.example" {
		t.Errorf("got %q want %q", got, "https://proxy.example")
	}
}

func TestExternalBaseFromRequest(t *testing.T) {
	h := New(Config{}) // no PublicURL
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "myserver.local:8080"
	got := h.externalBase(r)
	if got != "http://myserver.local:8080" {
		t.Errorf("got %q want %q", got, "http://myserver.local:8080")
	}
}

func TestExternalBaseXForwardedProtoHost(t *testing.T) {
	h := New(Config{})
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "internal:8080"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "cdn.example.com")
	got := h.externalBase(r)
	if got != "https://cdn.example.com" {
		t.Errorf("got %q want %q", got, "https://cdn.example.com")
	}
}

// ---------------------------------------------------------------------------
// buildProxyURL
// ---------------------------------------------------------------------------

func TestBuildProxyURLBasic(t *testing.T) {
	h := New(Config{PublicURL: "https://proxy.example"})
	dest := "https://cdn.example/seg001.ts"
	opts := &Options{}
	got := h.buildProxyURL("https://proxy.example", "/proxy/stream", dest, opts)

	encoded := base64.RawURLEncoding.EncodeToString([]byte(dest))
	want := "https://proxy.example/proxy/stream?d=" + encoded
	if got != want {
		t.Errorf("buildProxyURL basic:\n got  %s\n want %s", got, want)
	}
}

func TestBuildProxyURLWithPassword(t *testing.T) {
	h := New(Config{PublicURL: "https://proxy.example"})
	opts := &Options{APIPassword: "secret"}
	got := h.buildProxyURL("https://proxy.example", "/proxy/stream", "http://x.com/a.ts", opts)
	if !strings.Contains(got, "api_password=secret") {
		t.Errorf("api_password missing from URL: %s", got)
	}
}

func TestBuildProxyURLWithReqHeaders(t *testing.T) {
	h := New(Config{PublicURL: "https://proxy.example"})
	opts := &Options{
		ReqHeaders:  http.Header{"Authorization": []string{"Bearer tok"}},
		RespHeaders: make(http.Header),
	}
	got := h.buildProxyURL("https://proxy.example", "/proxy/stream", "http://x.com/a.ts", opts)
	if !strings.Contains(got, "h_Authorization=") {
		t.Errorf("request header missing from URL: %s", got)
	}
}

func TestBuildProxyURLNilOpts(t *testing.T) {
	h := New(Config{})
	// nil opts must not panic.
	got := h.buildProxyURL("https://p.example", "/proxy/stream", "http://x.com/a.ts", nil)
	if !strings.HasPrefix(got, "https://p.example/proxy/stream?d=") {
		t.Errorf("unexpected URL shape: %s", got)
	}
}

// ---------------------------------------------------------------------------
// resolveURL
// ---------------------------------------------------------------------------

func TestResolveURLRelative(t *testing.T) {
	base := "https://cdn.example/live/index.m3u8"
	ref := "seg001.ts"
	got := resolveURL(base, ref)
	want := "https://cdn.example/live/seg001.ts"
	if got != want {
		t.Errorf("resolveURL relative: got %q want %q", got, want)
	}
}

func TestResolveURLAbsolute(t *testing.T) {
	base := "https://cdn.example/live/index.m3u8"
	ref := "https://other.cdn/seg.ts"
	got := resolveURL(base, ref)
	if got != ref {
		t.Errorf("resolveURL absolute: got %q want %q", got, ref)
	}
}

func TestResolveURLRootRelative(t *testing.T) {
	base := "https://cdn.example/live/index.m3u8"
	ref := "/segs/001.ts"
	got := resolveURL(base, ref)
	want := "https://cdn.example/segs/001.ts"
	if got != want {
		t.Errorf("resolveURL root-relative: got %q want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// parseOptions
// ---------------------------------------------------------------------------

func TestParseOptionsPlainHTTPURL(t *testing.T) {
	h := New(Config{})
	r := httptest.NewRequest("GET", "/?d=https://cdn.example/v.ts", nil)
	opts, err := h.parseOptions(r)
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if opts.Dest != "https://cdn.example/v.ts" {
		t.Errorf("Dest: got %q", opts.Dest)
	}
}

func TestParseOptionsBase64Encoded(t *testing.T) {
	h := New(Config{})
	dest := "https://cdn.example/video.ts"
	enc := base64.RawURLEncoding.EncodeToString([]byte(dest))
	r := httptest.NewRequest("GET", "/?d="+enc, nil)
	opts, err := h.parseOptions(r)
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if opts.Dest != dest {
		t.Errorf("Dest after base64: got %q want %q", opts.Dest, dest)
	}
}

func TestParseOptionsHeaders(t *testing.T) {
	h := New(Config{})
	r := httptest.NewRequest("GET", "/?d=https://x.com/&h_Authorization=Bearer+tok&r_Cache-Control=no-cache", nil)
	opts, err := h.parseOptions(r)
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if opts.ReqHeaders.Get("Authorization") != "Bearer tok" {
		t.Errorf("ReqHeaders Authorization: got %q", opts.ReqHeaders.Get("Authorization"))
	}
	if opts.RespHeaders.Get("Cache-Control") != "no-cache" {
		t.Errorf("RespHeaders Cache-Control: got %q", opts.RespHeaders.Get("Cache-Control"))
	}
}

func TestParseOptionsAPIPassword(t *testing.T) {
	h := New(Config{})
	r := httptest.NewRequest("GET", "/?d=https://x.com/&api_password=pw123", nil)
	opts, err := h.parseOptions(r)
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if opts.APIPassword != "pw123" {
		t.Errorf("APIPassword: got %q", opts.APIPassword)
	}
}

func TestParseOptionsProxyParam(t *testing.T) {
	h := New(Config{})
	r := httptest.NewRequest("GET", "/?d=https://x.com/&proxy=socks5://localhost:1080", nil)
	opts, err := h.parseOptions(r)
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if opts.Proxy != "socks5://localhost:1080" {
		t.Errorf("Proxy: got %q", opts.Proxy)
	}
}

func TestParseOptionsInvalidProxyScheme(t *testing.T) {
	h := New(Config{})
	// ftp is not a valid proxy scheme → must be silently rejected.
	r := httptest.NewRequest("GET", "/?d=https://x.com/&proxy=ftp://bad-proxy", nil)
	opts, err := h.parseOptions(r)
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if opts.Proxy != "" {
		t.Errorf("invalid proxy scheme should be rejected, got %q", opts.Proxy)
	}
}

func TestParseOptionsEmptyD(t *testing.T) {
	h := New(Config{})
	r := httptest.NewRequest("GET", "/", nil)
	opts, err := h.parseOptions(r)
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if opts.Dest != "" {
		t.Errorf("empty d: Dest should be empty, got %q", opts.Dest)
	}
}

// ---------------------------------------------------------------------------
// HandleBase64
// ---------------------------------------------------------------------------

func TestHandleBase64Encode(t *testing.T) {
	h := New(Config{})
	input := "https://cdn.example/video.ts"
	enc := base64.RawURLEncoding.EncodeToString([]byte(input))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/base64/encode?d="+input, nil)
	h.HandleBase64(w, r, []string{"base64", "encode"})

	if w.Code != 200 {
		t.Errorf("status: got %d want 200", w.Code)
	}
	var res map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res["encoded_url"] != enc {
		t.Errorf("encoded_url: got %q want %q", res["encoded_url"], enc)
	}
}

func TestHandleBase64CheckValidBase64(t *testing.T) {
	h := New(Config{})
	input := "https://cdn.example/video.ts"
	enc := base64.RawURLEncoding.EncodeToString([]byte(input))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/base64/check?d="+enc, nil)
	h.HandleBase64(w, r, []string{"base64", "check"})

	if w.Code != 200 {
		t.Errorf("status: got %d want 200", w.Code)
	}
	var res map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res["is_base64"] != true {
		t.Errorf("is_base64: got %v want true", res["is_base64"])
	}
	if res["decoded"] != input {
		t.Errorf("decoded: got %q want %q", res["decoded"], input)
	}
}

func TestHandleBase64CheckNotBase64(t *testing.T) {
	h := New(Config{})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/base64/check?d=https://cdn.example/v.ts", nil)
	h.HandleBase64(w, r, []string{"base64", "check"})

	if w.Code != 200 {
		t.Errorf("status: got %d want 200", w.Code)
	}
	var res map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res["is_base64"] != false {
		t.Errorf("is_base64: got %v want false", res["is_base64"])
	}
}

func TestHandleBase64UnknownSubpath(t *testing.T) {
	h := New(Config{})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/base64/unknown", nil)
	h.HandleBase64(w, r, []string{"base64", "unknown"})
	if w.Code != http.StatusNotFound {
		t.Errorf("status: got %d want 404", w.Code)
	}
}

func TestHandleBase64TooShortSeg(t *testing.T) {
	h := New(Config{})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/base64", nil)
	h.HandleBase64(w, r, []string{"base64"}) // len(seg)==1 < 2
	if w.Code != http.StatusNotFound {
		t.Errorf("status: got %d want 404", w.Code)
	}
}

// ---------------------------------------------------------------------------
// HandleGenerateURL
// ---------------------------------------------------------------------------

func TestHandleGenerateURLSuccess(t *testing.T) {
	h := New(Config{Secret: testSecret})

	reqBody := `{"endpoint":"/proxy/stream","expiry_seconds":3600}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/generate_url", strings.NewReader(reqBody))
	r.RemoteAddr = "203.0.113.1:1234"
	h.HandleGenerateURL(w, r)

	if w.Code != 200 {
		t.Fatalf("status: got %d, body: %s", w.Code, w.Body.String())
	}
	var res map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	urlStr, ok := res["url"].(string)
	if !ok || urlStr == "" {
		t.Errorf("url field missing or empty: %v", res)
	}
	if !strings.Contains(urlStr, "/proxy/stream?token=") {
		t.Errorf("url shape unexpected: %q", urlStr)
	}
	if res["expires_at"] == nil {
		t.Error("expires_at field missing")
	}
}

func TestHandleGenerateURLNoSecret(t *testing.T) {
	h := New(Config{}) // no Secret

	reqBody := `{"endpoint":"/proxy/stream","expiry_seconds":3600}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/generate_url", strings.NewReader(reqBody))
	r.RemoteAddr = "203.0.113.1:1234"
	h.HandleGenerateURL(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d want 400", w.Code)
	}
}

func TestHandleGenerateURLBadJSON(t *testing.T) {
	h := New(Config{Secret: testSecret})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/generate_url", strings.NewReader("{bad json}"))
	r.RemoteAddr = "203.0.113.1:1234"
	h.HandleGenerateURL(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d want 400", w.Code)
	}
}

func TestHandleGenerateURLUnauthorized(t *testing.T) {
	h := New(Config{Secret: testSecret, Password: "required"})

	reqBody := `{"endpoint":"/proxy/stream","expiry_seconds":3600}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/generate_url?api_password=wrong", strings.NewReader(reqBody))
	r.RemoteAddr = "203.0.113.1:1234"
	h.HandleGenerateURL(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d want 401", w.Code)
	}
}

// Verify that the signed token returned by HandleGenerateURL passes verifyToken.
func TestHandleGenerateURLTokenVerifies(t *testing.T) {
	h := New(Config{Secret: testSecret})

	exp := time.Now().Add(time.Hour).Unix()
	reqBody := strings.NewReader(`{"endpoint":"/proxy/stream","expiry_seconds":3600}`)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/generate_url", reqBody)
	r.RemoteAddr = "203.0.113.1:1234"
	h.HandleGenerateURL(w, r)

	if w.Code != 200 {
		t.Fatalf("status: %d, body: %s", w.Code, w.Body.String())
	}
	var res map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	urlStr := res["url"].(string)

	// Extract token from the URL (after "?token=").
	idx := strings.Index(urlStr, "?token=")
	if idx < 0 {
		t.Fatalf("token not found in URL: %q", urlStr)
	}
	tokenStr := urlStr[idx+len("?token="):]

	tok, err := h.verifyToken(tokenStr, nil)
	if err != nil {
		t.Fatalf("verifyToken: %v", err)
	}
	if tok.Endpoint != "/proxy/stream" {
		t.Errorf("endpoint: got %q want /proxy/stream", tok.Endpoint)
	}
	if tok.Exp < exp {
		t.Errorf("expiry: got %d < expected %d", tok.Exp, exp)
	}
}

// ---------------------------------------------------------------------------
// Route dispatch
// ---------------------------------------------------------------------------

func TestRouteTooShort(t *testing.T) {
	h := New(Config{})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/proxy", nil)
	if h.Route(w, r, []string{"proxy"}) {
		t.Error("expected Route to return false for seg length 1")
	}
}

func TestRouteUnknown(t *testing.T) {
	h := New(Config{})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/proxy/unknown", nil)
	if h.Route(w, r, []string{"proxy", "unknown"}) {
		t.Error("expected Route to return false for unknown sub-path")
	}
}

func TestRouteHLS(t *testing.T) {
	h := New(Config{PublicURL: "https://proxy.example"})
	w := httptest.NewRecorder()
	// No ?d= param → bad request, but Route must return true (it handled the request).
	r := httptest.NewRequest("GET", "/proxy/hls/manifest.m3u8", nil)
	r.RemoteAddr = "203.0.113.1:1234"
	if !h.Route(w, r, []string{"proxy", "hls"}) {
		t.Error("expected Route to return true for /proxy/hls")
	}
	// Missing dest → 400.
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d want 400", w.Code)
	}
}

func TestRouteMPD(t *testing.T) {
	h := New(Config{PublicURL: "https://proxy.example"})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/proxy/mpd/manifest.mpd", nil)
	r.RemoteAddr = "203.0.113.1:1234"
	if !h.Route(w, r, []string{"proxy", "mpd"}) {
		t.Error("expected Route to return true for /proxy/mpd")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d want 400", w.Code)
	}
}

// ---------------------------------------------------------------------------
// hlsSegmentURLs
// ---------------------------------------------------------------------------

func TestHlsSegmentURLsBasic(t *testing.T) {
	base := "https://cdn.example/live/index.m3u8"
	playlist := "#EXTM3U\n" +
		"#EXT-X-TARGETDURATION:6\n" +
		"#EXTINF:6.000,\n" +
		"seg001.ts\n" +
		"#EXTINF:6.000,\n" +
		"seg002.ts\n" +
		"#EXT-X-ENDLIST\n"
	urls := hlsSegmentURLs(base, playlist, 10)
	if len(urls) != 2 {
		t.Fatalf("expected 2 URLs, got %d: %v", len(urls), urls)
	}
	if urls[0] != "https://cdn.example/live/seg001.ts" {
		t.Errorf("url[0]: got %q", urls[0])
	}
	if urls[1] != "https://cdn.example/live/seg002.ts" {
		t.Errorf("url[1]: got %q", urls[1])
	}
}

func TestHlsSegmentURLsMax(t *testing.T) {
	base := "https://cdn.example/live/index.m3u8"
	playlist := "#EXTM3U\n" +
		"seg001.ts\n" +
		"seg002.ts\n" +
		"seg003.ts\n"
	urls := hlsSegmentURLs(base, playlist, 2)
	if len(urls) != 2 {
		t.Errorf("expected max 2 URLs, got %d", len(urls))
	}
}

func TestHlsSegmentURLsSkipsM3U8(t *testing.T) {
	base := "https://cdn.example/live/index.m3u8"
	playlist := "#EXTM3U\n" +
		"variant.m3u8\n" + // should be skipped
		"seg001.ts\n"
	urls := hlsSegmentURLs(base, playlist, 10)
	if len(urls) != 1 {
		t.Fatalf("expected 1 URL (m3u8 skipped), got %d: %v", len(urls), urls)
	}
	if !strings.Contains(urls[0], "seg001.ts") {
		t.Errorf("url[0]: got %q", urls[0])
	}
}

func TestHlsSegmentURLsZeroMax(t *testing.T) {
	urls := hlsSegmentURLs("https://cdn.example/", "#EXTM3U\nseg.ts\n", 0)
	if len(urls) != 0 {
		t.Errorf("expected nil/empty for max=0, got %v", urls)
	}
}

// ---------------------------------------------------------------------------
// dashQueryEscape
// ---------------------------------------------------------------------------

func TestDashQueryEscapePreservesDollar(t *testing.T) {
	in := "https://cdn.example/seg-$Number$-$RepresentationID$.m4s"
	got := dashQueryEscape(in)
	if !strings.Contains(got, "$Number$") {
		t.Errorf("$Number$ was percent-encoded: %q", got)
	}
	if !strings.Contains(got, "$RepresentationID$") {
		t.Errorf("$RepresentationID$ was percent-encoded: %q", got)
	}
}

func TestDashQueryEscapePreservesParens(t *testing.T) {
	in := "https://cdn.example/seg(init).m4s"
	got := dashQueryEscape(in)
	if !strings.Contains(got, "(") || !strings.Contains(got, ")") {
		t.Errorf("parentheses were escaped: %q", got)
	}
}

func TestDashQueryEscapeEncodesSpecialChars(t *testing.T) {
	in := "https://cdn.example/seg?a=1&b=2 space"
	got := dashQueryEscape(in)
	// Spaces must be encoded.
	if strings.Contains(got, " ") {
		t.Errorf("space not encoded: %q", got)
	}
}

// ---------------------------------------------------------------------------
// Authorize (exported wrapper)
// ---------------------------------------------------------------------------

func TestAuthorizeExported(t *testing.T) {
	h := New(Config{Password: "pw"})
	r := httptest.NewRequest("GET", "/?api_password=pw", nil)
	r.RemoteAddr = "203.0.113.1:1234"
	if err := h.Authorize(r); err != nil {
		t.Errorf("Authorize: got %v want nil", err)
	}
}
