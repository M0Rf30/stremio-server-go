// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package streamproxy

import (
	"encoding/base64"
	"math/rand/v2"
	"net/url"
	"strings"
	"testing"
)

// slowResolve is the pre-optimisation resolution: url.Parse +
// ResolveReference + String for every URI.
func slowResolve(base *url.URL, ref string) (string, bool) {
	rv, err := url.Parse(ref)
	if err != nil {
		return ref, false
	}
	if hlsNonHTTPScheme(rv) {
		return ref, true
	}
	return base.ResolveReference(rv).String(), false
}

// TestHLSResolveFastPathEquivalence: whenever hlsPlainAbsolute accepts a URI,
// the fast path must return exactly what the full parse/resolve would.
func TestHLSResolveFastPathEquivalence(t *testing.T) {
	base, _ := url.Parse("https://cdn.example/live/sub/index.m3u8?x=1")
	cases := []string{
		"https://cdn.example/a/seg-1.ts",
		"https://cdn.example/a/seg-1.ts?token=R8v-PrGB&expires=1796730483",
		"http://CDN.Example:8080/A/B.TS?q=a+b&r=c;d",
		"https://cdn.example/a/./b.ts", "https://cdn.example/a/../b.ts", "https://cdn.example//a.ts",
		"https://cdn.example/a/b.ts?next=/./x", "https://cdn.example", "https://cdn.example?x",
		"https://cdn.example/a%20b.ts", "https://u:p@cdn.example/a.ts", "https://cdn.example/a.ts#frag",
		"HTTPS://cdn.example/a.ts", "https://cdn.example/a b.ts", "https://cdn.example/ä.ts",
		"https:///a.ts", "seg.ts", "/abs/seg.ts", "../up/seg.ts", "data:text/plain,x", "skd://key",
		"https://cdn.example/a.ts?q=%2F", "https://[::1]:443/a.ts", "https://cdn.example/a/?",
	}
	r := rand.New(rand.NewPCG(1, 2))
	alphabet := "abcAB09-_.~/?&=:;+,!$'()*@%# \\"
	for range 5000 {
		var b strings.Builder
		b.WriteString([]string{"https://", "http://", ""}[r.IntN(3)])
		b.WriteString("h.example")
		for range r.IntN(24) {
			b.WriteByte(alphabet[r.IntN(len(alphabet))])
		}
		cases = append(cases, b.String())
	}
	fast := 0
	for _, ref := range cases {
		wantAbs, wantSkip := slowResolve(base, ref)
		gotAbs, gotSkip := hlsResolve(base, nil, ref)
		if gotAbs != wantAbs || gotSkip != wantSkip {
			t.Errorf("hlsResolve(%q) = %q,%v; slow path = %q,%v", ref, gotAbs, gotSkip, wantAbs, wantSkip)
		}
		if hlsPlainAbsolute(ref) {
			fast++
		}
	}
	if fast == 0 {
		t.Fatal("fast path never taken")
	}
}

// TestHLSSegmentEndpointMatchesParse: the string-based extension lookup
// agrees with the url.Parse/path.Ext version it replaced.
func TestHLSSegmentEndpointMatchesParse(t *testing.T) {
	for abs, want := range map[string]string{
		"https://h/a/seg.ts":                "/proxy/stream/segment.ts",
		"https://h/a/seg.TS":                "/proxy/stream/segment.ts",
		"https://h/a/seg.m4s?x=.mp4":        "/proxy/stream/segment.m4s",
		"https://h/a/seg#x.mp4":             "/proxy/stream/segment.ts",
		"https://h.mp4":                     "/proxy/stream/segment.ts",
		"https://h.mp4/":                    "/proxy/stream/segment.ts",
		"https://h/dir.mp4/file":            "/proxy/stream/segment.ts",
		"relative/sub.vtt":                  "/proxy/stream/segment.vtt",
		"https://h/a/seg.jpg?token=a.b.mp4": "/proxy/stream/segment.ts",
	} {
		if got := hlsSegmentEndpoint(abs, "ts"); got != want {
			t.Errorf("hlsSegmentEndpoint(%q) = %q, want %q", abs, got, want)
		}
	}
	if got := hlsSegmentEndpoint("https://h/a", "fmp4"); got != "/proxy/stream/segment.fmp4" {
		t.Errorf("default = %q", got)
	}
	if got := hlsSegmentEndpoint("https://h/a", "zzz"); got != "/proxy/stream/segment.zzz" {
		t.Errorf("unknown default = %q", got)
	}
}

// TestProxyURLBuilderBase64: the chunked encoder must equal
// RawURLEncoding.EncodeToString for every length (chunk boundaries, padding).
func TestProxyURLBuilderBase64(t *testing.T) {
	h := New(Config{})
	defer h.Close()
	pb := h.newProxyURLBuilder(nil)
	var src strings.Builder
	for n := range 200 {
		dest := src.String()
		got := pb.build("", "/e", dest)
		want := "/e?d=" + base64.RawURLEncoding.EncodeToString([]byte(dest))
		if got != want {
			t.Fatalf("len %d: got %q want %q", n, got, want)
		}
		src.WriteByte(byte(0x20 + n%95))
	}
}

// TestProxyURLBuilderAllocs: one allocation per URL in password mode.
func TestProxyURLBuilderAllocs(t *testing.T) {
	h := New(Config{Password: "pw"})
	defer h.Close()
	opts := &Options{APIPassword: "pw", ReqHeaders: map[string][]string{"Referer": {"https://player.example/"}}}
	pb := h.newProxyURLBuilder(opts)
	dest := "https://sc-u11-01.cdn.example/hls/3/8/cf/8cf686c9/video/480p/0001-0002?token=R8v-PrGBJbrHdTSVOptAXw&expires=1796730483"
	if n := testing.AllocsPerRun(100, func() { _ = pb.build("https://ext.example", "/proxy/stream/segment.ts", dest) }); n != 1 {
		t.Errorf("build allocs = %v, want 1", n)
	}
	if n := testing.AllocsPerRun(100, func() { _ = hlsSegmentEndpoint(dest, "ts") }); n != 0 {
		t.Errorf("hlsSegmentEndpoint allocs = %v, want 0", n)
	}
}
