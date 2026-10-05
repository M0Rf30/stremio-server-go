// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/M0Rf30/stremio-server-go/internal/types"
)

// landingLocation runs handleLanding for the given target URL and returns the
// Location header it produced. peer is the immediate TCP peer (r.RemoteAddr);
// an empty value leaves httptest's default (a public address), so forwarded
// headers are ignored unless a trusted peer is given.
func landingLocation(t *testing.T, cfg types.Config, target, peer string, headers map[string]string) string {
	t.Helper()
	s := &server{cfg: cfg}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if peer != "" {
		req.RemoteAddr = peer
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.handleLanding(rec, req)
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("handleLanding status = %d; want %d", rec.Code, http.StatusTemporaryRedirect)
	}
	return rec.Header().Get("Location")
}

// landingWant is the expected Location: the web UI with both the shell's
// `streamingServer` and stremio-web's `streamingServerUrl` set to base.
func landingWant(webUI, base string) string {
	escaped := url.QueryEscape(base)
	return webUI + "?streamingServer=" + escaped + "&streamingServerUrl=" + escaped
}

func TestLandingUsesPublicURL(t *testing.T) {
	cfg := types.Config{WebUI: "https://web.stremio.com/", PublicURL: "https://stremio.example.com"}
	got := landingLocation(t, cfg, "http://127.0.0.1:11470/", "", nil)
	if want := landingWant(cfg.WebUI, "https://stremio.example.com"); got != want {
		t.Errorf("Location = %q; want %q", got, want)
	}
}

func TestLandingHonoursForwardedHeaders(t *testing.T) {
	cfg := types.Config{WebUI: "https://web.stremio.com/"}
	got := landingLocation(t, cfg, "http://stremio.example.com/", "127.0.0.1:1234", map[string]string{
		"X-Forwarded-Proto": "https",
		"X-Forwarded-Host":  "stremio.example.com",
	})
	if want := landingWant(cfg.WebUI, "https://stremio.example.com"); got != want {
		t.Errorf("Location = %q; want %q", got, want)
	}
}

// TestLandingIgnoresForwardedHeadersFromPublicPeer guards the trusted-proxy
// rule: a public client must not be able to spoof the base via
// X-Forwarded-Proto/Host.
func TestLandingIgnoresForwardedHeadersFromPublicPeer(t *testing.T) {
	cfg := types.Config{WebUI: "https://web.stremio.com/"}
	got := landingLocation(t, cfg, "http://stremio.example.com/", "203.0.113.1:1234", map[string]string{
		"X-Forwarded-Proto": "https",
		"X-Forwarded-Host":  "evil.example.com",
	})
	if want := landingWant(cfg.WebUI, "http://stremio.example.com"); got != want {
		t.Errorf("Location = %q; want %q (forwarded headers from a public peer must be ignored)", got, want)
	}
}

func TestLandingUsesTLSWhenPresent(t *testing.T) {
	cfg := types.Config{WebUI: "https://web.stremio.com/"}
	got := landingLocation(t, cfg, "https://stremio.example.com/", "", nil)
	if want := landingWant(cfg.WebUI, "https://stremio.example.com"); got != want {
		t.Errorf("Location = %q; want %q", got, want)
	}
}

func TestLandingFallsBackToRequest(t *testing.T) {
	cfg := types.Config{WebUI: "https://web.stremio.com/"}
	got := landingLocation(t, cfg, "http://127.0.0.1:11470/", "", nil)
	if want := landingWant(cfg.WebUI, "http://127.0.0.1:11470"); got != want {
		t.Errorf("Location = %q; want %q", got, want)
	}
}

func TestBaseURLUsesPublicURL(t *testing.T) {
	s := &server{cfg: types.Config{HTTPPort: 11470, PublicURL: "https://stremio.example.com"}}
	if got := s.baseURL(); got != "https://stremio.example.com" {
		t.Errorf("baseURL = %q; want %q", got, "https://stremio.example.com")
	}
}

// TestLandingTakesFirstForwardedProto guards against a comma-joined
// X-Forwarded-Proto ("https, http") producing a malformed base: only the first
// element counts.
func TestLandingTakesFirstForwardedProto(t *testing.T) {
	cfg := types.Config{WebUI: "https://web.stremio.com/"}
	got := landingLocation(t, cfg, "http://stremio.example.com/", "127.0.0.1:1234", map[string]string{
		"X-Forwarded-Proto": "https, http",
	})
	if want := landingWant(cfg.WebUI, "https://stremio.example.com"); got != want {
		t.Errorf("Location = %q; want %q", got, want)
	}
}

// TestLandingIgnoresBogusForwardedProto accepts only http/https; anything else
// falls back to the request scheme.
func TestLandingIgnoresBogusForwardedProto(t *testing.T) {
	cfg := types.Config{WebUI: "https://web.stremio.com/"}
	got := landingLocation(t, cfg, "http://stremio.example.com/", "127.0.0.1:1234", map[string]string{
		"X-Forwarded-Proto": "ftp",
	})
	if want := landingWant(cfg.WebUI, "http://stremio.example.com"); got != want {
		t.Errorf("Location = %q; want %q", got, want)
	}
}

// TestSettingsBaseURLUsesPublicURL is the handler-level check that
// STREMIO_PUBLIC_URL also drives the /settings baseUrl (not only the landing
// redirect).
func TestSettingsBaseURLUsesPublicURL(t *testing.T) {
	h := newHandlerWithCfg(t, func(c *types.Config) {
		c.HTTPPort = 11470
		c.PublicURL = "https://stremio.example.com"
	})
	rec := serve(t, h, http.MethodGet, "/settings", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("/settings = %d; want 200", rec.Code)
	}
	if got := decodeJSON(t, rec.Body.Bytes())["baseUrl"]; got != "https://stremio.example.com" {
		t.Errorf("/settings baseUrl = %v; want the public URL", got)
	}
}
