// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/M0Rf30/stremio-server-go/internal/types"
)

func serveWithHeaders(h http.Handler, path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestCrossSiteOriginlessGETBlocked: a browser-originated cross-site GET with
// no Origin (e.g. <img src=".../removeAll">) must be rejected on side-effecting
// routes, while header-less clients and same-origin/same-site/none still work.
func TestCrossSiteOriginlessGETBlocked(t *testing.T) {
	paths := []string{
		"/removeAll",
		"/" + testIH + "/remove",
		"/" + testIH + "/create",
		"/get-https?authKey=x&ipAddress=1.2.3.4",
		"/casting/dev1/player/stop",
		"/casting",
	}
	h := newHandler(t, testEngine())
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			rec := serveWithHeaders(h, p, map[string]string{"Sec-Fetch-Site": "cross-site"})
			if rec.Code != http.StatusForbidden {
				t.Fatalf("cross-site origin-less GET %s = %d; want 403", p, rec.Code)
			}
			for _, site := range []string{"same-origin", "same-site", "none", ""} {
				hdr := map[string]string{}
				if site != "" {
					hdr["Sec-Fetch-Site"] = site
				}
				rec := serveWithHeaders(h, p, hdr)
				if rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "cross-site") {
					t.Errorf("Sec-Fetch-Site=%q GET %s wrongly blocked as cross-site", site, p)
				}
			}
		})
	}
}

func TestCrossSiteRemoveAllWithoutFetchMetadataSucceeds(t *testing.T) {
	h := newHandler(t)
	if rec := serve(t, h, http.MethodGet, "/removeAll", nil); rec.Code != http.StatusOK {
		t.Fatalf("/removeAll without Sec-Fetch-Site = %d; want 200", rec.Code)
	}
	for _, site := range []string{"same-origin", "same-site", "none"} {
		rec := serveWithHeaders(h, "/removeAll", map[string]string{"Sec-Fetch-Site": site})
		if rec.Code != http.StatusOK {
			t.Errorf("/removeAll with Sec-Fetch-Site=%s = %d; want 200", site, rec.Code)
		}
	}
}

// Media playback (<video src>) is cross-site and Origin-less by design and must
// keep working, as must non-side-effecting reads.
func TestCrossSiteOriginlessMediaAndReadsAllowed(t *testing.T) {
	h := newHandler(t, testEngine())
	for _, p := range []string{"/" + testIH + "/0", "/heartbeat", "/hlsv2/abc/stream_0/seg0.ts", "/hlsv2/abc/master.m3u8?mediaURL=http%3A%2F%2Fexample.com%2Fv.mkv", "/yt/dQw4w9WgXcQ"} {
		rec := serveWithHeaders(h, p, map[string]string{"Sec-Fetch-Site": "cross-site"})
		if rec.Code == http.StatusForbidden {
			t.Errorf("cross-site GET %s = 403; media/read routes must stay allowed", p)
		}
	}
}

func TestCrossSiteAllowAllOriginsLegacy(t *testing.T) {
	h := newHandlerWithCfg(t, func(c *types.Config) { c.AllowAllOrigins = true })
	rec := serveWithHeaders(h, "/removeAll", map[string]string{"Sec-Fetch-Site": "cross-site"})
	if rec.Code != http.StatusOK {
		t.Fatalf("AllowAllOrigins /removeAll cross-site = %d; want 200", rec.Code)
	}
}

func TestCrossSiteWithAllowedOriginNotBlocked(t *testing.T) {
	h := newHandler(t)
	rec := serveWithHeaders(h, "/removeAll", map[string]string{
		"Sec-Fetch-Site": "cross-site",
		"Origin":         "https://web.stremio.com",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("allowlisted Origin cross-site /removeAll = %d; want 200", rec.Code)
	}
}

func assertActiveContentGuards(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q; want nosniff", got)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != "sandbox" {
		t.Errorf("Content-Security-Policy = %q; want sandbox", got)
	}
}

func TestHandleStreamSetsActiveContentGuards(t *testing.T) {
	h := newHandler(t, testEngine())
	rec := serve(t, h, http.MethodGet, "/"+testIH+"/0", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("stream status = %d; want 200", rec.Code)
	}
	assertActiveContentGuards(t, rec)
}

func TestHandleProxySetsActiveContentGuardsAndIgnoresOverrides(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Security-Policy", "default-src *")
		w.Header().Set("X-Content-Type-Options", "")
		_, _ = w.Write([]byte("<script>1</script>"))
	}))
	defer upstream.Close()
	enc := url.QueryEscape(upstream.URL)
	h := newHandler(t)

	rec := serve(t, h, http.MethodGet, "/proxy/d="+enc+"/x.html", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy status = %d; want 200", rec.Code)
	}
	assertActiveContentGuards(t, rec)

	// r= overrides must not be able to drop or weaken the guards.
	ov := "&r=Content-Security-Policy%3Adefault-src%20*&r=x-content-type-options%3A&r=X-Custom%3Aok"
	rec = serve(t, h, http.MethodGet, "/proxy/d="+enc+ov+"/x.html", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy status = %d; want 200", rec.Code)
	}
	assertActiveContentGuards(t, rec)
	if got := rec.Header().Get("X-Custom"); got != "ok" {
		t.Errorf("non-guarded r= override X-Custom = %q; want ok", got)
	}
}
