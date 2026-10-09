// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package streamproxy

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/M0Rf30/stremio-server-go/internal/netguard"
)

// TestPrivatePolicy pins who may reach private destinations. The signing
// secret alone (auto-generated on every install) must not force blocking, or
// STREMIO_PROXY_ALLOW_PRIVATE would be a no-op.
func TestPrivatePolicy(t *testing.T) {
	_, acl, _ := net.ParseCIDR("203.0.113.0/24")
	allow, _ := netguard.ParseAllow("127.0.0.1")
	cases := []struct {
		name     string
		cfg      Config
		loopback bool // ValidateDest("http://127.0.0.1/") allowed
		lan      bool // ValidateDest("http://10.0.0.9/") allowed
	}{
		{"default (allow-private unset)", Config{BlockPrivate: true, Secret: testSecret}, false, false},
		{"allow-private + secret only", Config{Secret: testSecret}, true, true},
		{"allow-private + password", Config{Secret: testSecret, Password: "pw"}, false, false},
		{"allow-private + ip acl", Config{Secret: testSecret, IPACL: []*net.IPNet{acl}}, false, false},
		{"password + allowlist", Config{Secret: testSecret, Password: "pw", PrivateAllow: allow}, true, false},
		{"default + allowlist", Config{BlockPrivate: true, PrivateAllow: allow}, true, false},
	}
	for _, tc := range cases {
		h := New(tc.cfg)
		if got := h.ValidateDest("http://127.0.0.1/x") == nil; got != tc.loopback {
			t.Errorf("%s: loopback allowed=%v want %v", tc.name, got, tc.loopback)
		}
		if got := h.ValidateDest("http://10.0.0.9/x") == nil; got != tc.lan {
			t.Errorf("%s: lan allowed=%v want %v", tc.name, got, tc.lan)
		}
		if h.ValidateDest("http://169.254.169.254/latest") == nil {
			t.Errorf("%s: cloud metadata allowed", tc.name)
		}
		if got := h.validateProxyHost("http://127.0.0.1:1080") == nil; got != tc.loopback {
			t.Errorf("%s: loopback proxy host allowed=%v want %v", tc.name, got, tc.loopback)
		}
		h.Close()
	}
}

// TestPrivateAllowServes: with a password set (exposed proxy), an
// allowlisted loopback upstream is actually streamed end to end.
func TestPrivateAllowServes(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("LAN-MEDIA"))
	}))
	defer up.Close()
	dest := "/proxy/stream?api_password=pw&d=" + url.QueryEscape(up.URL+"/v.mp4")

	allow, _ := netguard.ParseAllow("127.0.0.1")
	for _, tc := range []struct {
		name  string
		allow *netguard.Allow
		code  int
	}{
		{"not allowlisted", nil, http.StatusForbidden},
		{"allowlisted", allow, http.StatusOK},
	} {
		h := New(Config{Password: "pw", Secret: testSecret, PrivateAllow: tc.allow})
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", dest, nil)
		r.RemoteAddr = "203.0.113.7:5000"
		h.serveStream(w, r)
		if w.Code != tc.code {
			t.Errorf("%s: %d %q, want %d", tc.name, w.Code, w.Body.String(), tc.code)
		}
		h.Close()
	}
}
