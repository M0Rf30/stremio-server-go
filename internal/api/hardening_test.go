// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/M0Rf30/stremio-server-go/internal/streamproxy"
	"github.com/M0Rf30/stremio-server-go/internal/types"
)

func TestHostAllowed_RebindingBlocked(t *testing.T) {
	h := newHandler(t)
	for host, want := range map[string]int{
		"evil.attacker.net":  http.StatusForbidden,
		"localhost:11470":    http.StatusOK,
		"127.0.0.1:11470":    http.StatusOK,
		"192.168.1.20:11470": http.StatusOK,
		"nas.local:11470":    http.StatusOK,
		"[::1]:11470":        http.StatusOK,
	} {
		req := httptest.NewRequest(http.MethodGet, "/heartbeat", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %q = %d; want %d", host, rec.Code, want)
		}
	}
	// media routes are exempt
	req := httptest.NewRequest(http.MethodGet, "/"+testIH+"/0", nil)
	req.Host = "evil.attacker.net"
	rec := httptest.NewRecorder()
	newHandler(t, testEngine()).ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Errorf("media route must not be host-gated")
	}
}

func TestHostAllowed_PublicURL(t *testing.T) {
	h := newHandlerWithCfg(t, func(c *types.Config) { c.PublicURL = "https://stremio.example.org" })
	req := httptest.NewRequest(http.MethodGet, "/heartbeat", nil)
	req.Host = "stremio.example.org"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("public URL host = %d", rec.Code)
	}
}

func TestOriginForeignLocalPortRejected(t *testing.T) {
	if originAllowed(types.Config{HTTPPort: 11470}, "http://localhost:9999") {
		t.Error("foreign localhost port must be rejected")
	}
	if !originAllowed(types.Config{HTTPPort: 11470}, "http://localhost:11470") {
		t.Error("own port must be allowed")
	}
	if !originAllowed(types.Config{HTTPPort: 11470, AllowedOrigins: []string{"localhost:9999"}}, "http://localhost:9999") {
		t.Error("configured extra must be allowed")
	}
}

func TestSettingsPostOriginlessRemoteRefused(t *testing.T) {
	h := newHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(`{"cacheSize":1}`))
	req.RemoteAddr = "192.0.2.9:1111"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d; want 403", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(`{"cacheSize":1}`))
	req.RemoteAddr = "192.0.2.9:1111"
	req.Header.Set("Origin", "https://web.stremio.com")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("with Origin status = %d; want 200", rec.Code)
	}
}

func TestSettingsPostDropsUnknownKeys(t *testing.T) {
	ss := &fakeSS{}
	h := New(newFakeEM(), ss, &fakeProber{}, types.Config{HTTPPort: 11470})
	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(`{"cacheSize":5,"evil":"x"}`))
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if ss.Get("evil") != nil {
		t.Error("unknown key persisted")
	}
}

func TestSideEffectingRouteCoverage(t *testing.T) {
	for _, p := range []string{"/probe", "/tracks/x", "/opensubHash", "/subtitlesTracks", "/nzb/create", "/ftp/create", "/bitmagnet/x", "/torznab/x", "/zip/create", "/hlsv2/probe", "/proxy/d=x/y", "/yt/abc"} {
		r := httptest.NewRequest(http.MethodGet, p, nil)
		if !sideEffectingRoute(r) {
			t.Errorf("%s should be side-effecting", p)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/zip/stream/k/f", nil)
	if sideEffectingRoute(r) {
		t.Error("archive stream must stay playable")
	}
}

func TestProxyBlocksPrivateByDefault(t *testing.T) {
	t.Setenv("STREMIO_PROXY_ALLOW_PRIVATE", "")
	h := newHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/proxy/d=http%3A%2F%2F127.0.0.1%3A1/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d; want 403", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/proxy/d=http%3A%2F%2Fexample.org/x", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d; want 405", rec.Code)
	}
}

func TestProxyHeaderFilters(t *testing.T) {
	for _, n := range []string{"Set-Cookie", "Refresh", "Location", "Content-Security-Policy", "Access-Control-Allow-Origin"} {
		if !blockedResp(n) {
			t.Errorf("%s should be blocked", n)
		}
	}
}

func blockedResp(n string) bool { return streamproxy.BlockedRespHeader(http.CanonicalHeaderKey(n)) }
