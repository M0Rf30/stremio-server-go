// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package streamproxy

import (
	"encoding/base64"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// dashProxiedDest extracts and decodes the destination from the first
// /proxy/stream?d=<base64> URL found in s.
func dashProxiedDest(t *testing.T, s string) string {
	t.Helper()
	const marker = "/proxy/stream?d="
	i := strings.Index(s, marker)
	if i < 0 {
		t.Fatalf("no proxied URL in %q", s)
	}
	rest := s[i+len(marker):]
	if j := strings.IndexAny(rest, `&"<`); j >= 0 {
		rest = rest[:j]
	}
	b, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		t.Fatalf("decode %q: %v", rest, err)
	}
	return string(b)
}

func TestDashRewriteBaseURLChain(t *testing.T) {
	const mpd = `<MPD>
<BaseURL>../media/</BaseURL>
<BaseURL>https://failover.example/ignored/</BaseURL>
<Period>
<BaseURL>p1/</BaseURL>
<AdaptationSet id="a">
<BaseURL>v/</BaseURL>
<SegmentTemplate initialization="aset-init.mp4" media="aset-$Number$.m4s"/>
<Representation id="r1"><BaseURL>r1/</BaseURL>
<SegmentTemplate initialization="init.mp4" media="seg-$Number$.m4s"/>
</Representation>
<Representation id="r2">
<SegmentList><Initialization sourceURL="init2.mp4"/><SegmentURL media="c1.m4s"/></SegmentList>
</Representation>
</AdaptationSet>
<AdaptationSet id="b">
<Representation id="r3"><SegmentList><SegmentURL media="c3.m4s"/></SegmentList></Representation>
</AdaptationSet>
</Period>
<Period>
<BaseURL>https://other.example/abs/</BaseURL>
<AdaptationSet><Representation><SegmentList><SegmentURL media="c4.m4s"/></SegmentList></Representation></AdaptationSet>
</Period>
</MPD>`
	const dest = "https://origin.example/a/b/master.mpd"

	h := dashNewTestHandler()
	req := httptest.NewRequest("GET", "/proxy/mpd/manifest.m3u8", nil)
	opts := &Options{Dest: dest}
	out := string(dashRewrite(h, req, opts, []byte(mpd)))

	dec := xml.NewDecoder(strings.NewReader(out))
	if err := func() error {
		for {
			if _, err := dec.Token(); err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
		}
	}(); err != nil {
		t.Fatalf("output not well-formed: %v\n%s", err, out)
	}

	// Template attributes: plain-escaped absolute destination.
	for _, want := range []string{
		"https://origin.example/a/media/p1/v/aset-init.mp4",
		"https://origin.example/a/media/p1/v/aset-$Number$.m4s",
		"https://origin.example/a/media/p1/v/r1/init.mp4",
		"https://origin.example/a/media/p1/v/r1/seg-$Number$.m4s",
	} {
		if !strings.Contains(out, "d="+dashQueryEscape(want)) {
			t.Errorf("missing template destination %q in:\n%s", want, out)
		}
	}

	// SegmentList entries: base64 destinations.
	for _, want := range []string{
		"https://origin.example/a/media/p1/v/init2.mp4", // sibling Representation: r1's BaseURL must not leak
		"https://origin.example/a/media/p1/v/c1.m4s",
		"https://origin.example/a/media/p1/c3.m4s", // sibling AdaptationSet: v/ must not leak
		"https://other.example/abs/c4.m4s",         // second Period: first Period's base must not leak
	} {
		enc := base64.RawURLEncoding.EncodeToString([]byte(want))
		if !strings.Contains(out, "d="+enc) {
			t.Errorf("missing destination %q in:\n%s", want, out)
		}
	}

	// The BaseURL elements themselves are chained too (first BaseURL only
	// defines the base; the failover one still gets proxied).
	for _, want := range []string{
		"https://origin.example/a/media/",
		"https://origin.example/a/media/p1/",
		"https://origin.example/a/media/p1/v/",
		"https://origin.example/a/media/p1/v/r1/",
	} {
		enc := base64.RawURLEncoding.EncodeToString([]byte(want))
		if !strings.Contains(out, "d="+enc) {
			t.Errorf("missing BaseURL destination %q in:\n%s", want, out)
		}
	}
}

func TestDashRewriteBaseURLWithoutBaseUsesMPDURL(t *testing.T) {
	const mpd = `<MPD><Period><AdaptationSet><Representation><SegmentList><SegmentURL media="c.m4s"/></SegmentList></Representation></AdaptationSet></Period></MPD>`
	h := dashNewTestHandler()
	req := httptest.NewRequest("GET", "/proxy/mpd/manifest.m3u8", nil)
	out := string(dashRewrite(h, req, &Options{Dest: "https://origin.example/x/m.mpd"}, []byte(mpd)))
	if got := dashProxiedDest(t, out); got != "https://origin.example/x/c.m4s" {
		t.Errorf("dest = %q", got)
	}
}

func TestDashServeUpstreamNon2xx(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `<MPD><BaseURL>x/</BaseURL></MPD>`, code)
		}))
		h := New(Config{PublicURL: "https://ext.example", Client: srv.Client()})
		req := httptest.NewRequest("GET", "/proxy/mpd/manifest.m3u8?d="+url.QueryEscape(srv.URL+"/x.mpd"), nil)
		w := httptest.NewRecorder()
		dashServe(h, w, req)
		srv.Close()
		if w.Code != http.StatusBadGateway {
			t.Errorf("upstream %d: got status %d want 502", code, w.Code)
		}
		if strings.Contains(w.Body.String(), "/proxy/stream") || strings.Contains(w.Body.String(), "<MPD") {
			t.Errorf("upstream %d: error body was passed/rewritten as a manifest: %s", code, w.Body.String())
		}
	}
}

func TestDashServeOKSecurityHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<MPD><Period/></MPD>`)
	}))
	defer srv.Close()
	h := New(Config{PublicURL: "https://ext.example", Client: srv.Client()})
	req := httptest.NewRequest("GET", "/proxy/mpd/manifest.m3u8?d="+url.QueryEscape(srv.URL+"/x.mpd"), nil)
	w := httptest.NewRecorder()
	dashServe(h, w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Content-Security-Policy") != "sandbox" {
		t.Errorf("security headers missing: %v", w.Header())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/dash+xml" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestDashBuildTemplateURLSubToken(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	h := New(Config{PublicURL: "https://ext.example", Password: "pw", Secret: secret})
	opts := &Options{Dest: "https://cdn.example/m.mpd", subTokenExp: time.Now().Add(time.Hour).Unix()}

	got := dashBuildTemplateURL(h, "https://ext.example", "https://cdn.example/seg-$Number$.m4s", opts)
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	tok := u.Query().Get("token")
	if tok == "" {
		t.Fatalf("no token minted for token-authed request: %s", got)
	}
	if strings.Contains(got, "api_password") {
		t.Errorf("password leaked: %s", got)
	}
	parsed, err := h.verifyToken(tok, nil)
	if err != nil {
		t.Fatalf("minted token does not verify: %v", err)
	}
	if parsed.Endpoint != "/proxy/stream" {
		t.Errorf("token endpoint = %q", parsed.Endpoint)
	}

	// Not token-authorised and no password: no credential added.
	plain := dashBuildTemplateURL(h, "https://ext.example", "https://cdn.example/s.m4s", &Options{})
	if strings.Contains(plain, "token=") || strings.Contains(plain, "api_password=") {
		t.Errorf("unexpected credential: %s", plain)
	}
}
