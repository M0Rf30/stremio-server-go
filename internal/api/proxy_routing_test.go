package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestHandlerProxy_CoreGeneratedURL is a regression test for the routing bug
// where net/url's automatic percent-decoding of r.URL.Path shattered the
// stremio-core-generated /proxy/<opts>/<path> opts segment (which itself
// contains percent-encoded "%2F"/"%3A"/"%2B") across strings.Split, so
// url.ParseQuery(seg[1]) never recovered the origin or headers.
//
// Wire-form fixtures are taken verbatim from stremio-core's
// stream_deep_links.rs unit tests (stream_deep_links_http_with_request_headers
// and stream_deep_links_http_with_request_response_headers_and_query_params),
// with "domain.root" substituted for a local httptest server address.
func TestHandlerProxy_CoreGeneratedURL(t *testing.T) {
	var gotAuth, gotPath, gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	encOrigin := url.QueryEscape(upstream.URL)

	tests := []struct {
		name      string
		reqPath   string
		wantQuery string
		wantCT    string // expected response Content-Type ("" fixture has no r= override)
	}{
		{
			// stream_deep_links_http_with_request_headers
			name:    "d + h opts segment",
			reqPath: "/proxy/d=" + encOrigin + "&h=Authorization%3Amy%2Btoken/some/path",
			wantCT:  "text/plain",
		},
		{
			// stream_deep_links_http_with_request_response_headers_and_query_params
			name:      "d + h + r opts segment with trailing query",
			reqPath:   "/proxy/d=" + encOrigin + "&h=Authorization%3Amy%2Btoken&r=Content-Type%3Aapplication%2Fxml/some/path?param=some&foo=bar",
			wantQuery: "param=some&foo=bar",
			wantCT:    "application/xml",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotAuth, gotPath, gotQuery = "", "", ""
			h := newHandler(t)
			rec := serve(t, h, http.MethodGet, tc.reqPath, nil)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; want 200; body=%s", rec.Code, rec.Body.String())
			}
			if gotAuth != "my+token" {
				t.Errorf("upstream Authorization header = %q; want %q (origin/header recovery broken)", gotAuth, "my+token")
			}
			if gotPath != "/some/path" {
				t.Errorf("upstream saw path %q; want %q", gotPath, "/some/path")
			}
			if gotQuery != tc.wantQuery {
				t.Errorf("upstream saw query %q; want %q", gotQuery, tc.wantQuery)
			}
			if got := rec.Header().Get("Content-Type"); got != tc.wantCT {
				t.Errorf("response Content-Type = %q; want %q", got, tc.wantCT)
			}
		})
	}
}
