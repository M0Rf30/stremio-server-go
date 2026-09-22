package streamproxy

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ---------------------------------------------------------------------------
// validateProxyHost — SSRF guard for the client-supplied "proxy" query param
// ---------------------------------------------------------------------------

func TestValidateProxyHostBlocksPrivateWhenProtected(t *testing.T) {
	h := New(Config{Password: "s"})
	if err := h.validateProxyHost("http://127.0.0.1:9999"); err == nil {
		t.Fatal("expected error for private proxy host on protected handler, got nil")
	}
}

func TestValidateProxyHostAllowsPrivateWhenUnprotected(t *testing.T) {
	h := New(Config{})
	if err := h.validateProxyHost("http://127.0.0.1:9999"); err != nil {
		t.Fatalf("private proxy host on unprotected handler should be allowed: %v", err)
	}
}

func TestValidateProxyHostBlocksCloudMetadataAlways(t *testing.T) {
	// Cloud-metadata must be blocked even on an otherwise-unprotected handler.
	h := New(Config{})
	if err := h.validateProxyHost("http://169.254.169.254:80"); err == nil {
		t.Fatal("expected error for cloud-metadata proxy host, got nil")
	}
}

func TestValidateProxyHostAllowsPublicHost(t *testing.T) {
	h := New(Config{Password: "s"})
	if err := h.validateProxyHost("http://203.0.113.5:1080"); err != nil {
		t.Fatalf("public proxy host should be allowed: %v", err)
	}
}

func TestValidateProxyHostSocks5SchemeBlocksPrivate(t *testing.T) {
	h := New(Config{Password: "s"})
	if err := h.validateProxyHost("socks5://127.0.0.1:1080"); err == nil {
		t.Fatal("expected error for private socks5 proxy host on protected handler, got nil")
	}
	if err := h.validateProxyHost("socks5h://127.0.0.1:1080"); err == nil {
		t.Fatal("expected error for private socks5h proxy host on protected handler, got nil")
	}
}

func TestValidateProxyHostSocks5SchemeAllowsPublic(t *testing.T) {
	h := New(Config{Password: "s"})
	if err := h.validateProxyHost("socks5://203.0.113.5:1080"); err != nil {
		t.Fatalf("public socks5 proxy host should be allowed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// clientFor / buildProxyClient — guarded dialer wiring
// ---------------------------------------------------------------------------

// TestClientForBuildsGuardedClientForProxyHost confirms clientFor actually
// builds a dedicated proxy client (not the base client) for a valid
// proxyURL, for both the HTTP and SOCKS5 branches of buildProxyClient.
func TestClientForBuildsGuardedClientForProxyHost(t *testing.T) {
	h := New(Config{Password: "s"})
	if c := h.clientFor("http://203.0.113.6:1080"); c == h.cfg.Client {
		t.Fatal("expected a dedicated proxy client for an http proxy URL, got the base client")
	}
	if c := h.clientFor("socks5://203.0.113.6:1080"); c == h.cfg.Client {
		t.Fatal("expected a dedicated proxy client for a socks5 proxy URL, got the base client")
	}
}

// TestBuildProxyClientDialControlBlocksPrivateHTTP proves the HTTP branch of
// buildProxyClient wires netguard.DialControl onto the dialer that connects
// to the proxy host itself: even if a caller built a blockPrivate=true
// client directly (bypassing validateProxyHost's pre-flight check), the
// actual TCP dial to a private/loopback proxy address is refused, so the
// victim server's handler is never invoked.
func TestBuildProxyClientDialControlBlocksPrivateHTTP(t *testing.T) {
	invoked := false
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		invoked = true
		w.WriteHeader(http.StatusOK)
	}))
	defer victim.Close()

	c, err := buildProxyClient(victim.URL, true) // blockPrivate=true
	if err != nil {
		t.Fatalf("buildProxyClient: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://203.0.113.5/x", nil)
	if _, doErr := c.Do(req); doErr == nil {
		t.Fatal("expected the dial to the private proxy host to be blocked, got nil error")
	}
	if invoked {
		t.Fatal("victim server handler was invoked; DialControl did not block the private dial")
	}
}

// TestBuildProxyClientDialControlAllowsPrivateWhenUnguarded confirms the
// blockPrivate=false path still reaches a private/loopback proxy host,
// proving DialControl only rejects private addresses when actually asked to.
func TestBuildProxyClientDialControlAllowsPrivateWhenUnguarded(t *testing.T) {
	invoked := false
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		invoked = true
		w.WriteHeader(http.StatusOK)
	}))
	defer victim.Close()

	c, err := buildProxyClient(victim.URL, false) // blockPrivate=false
	if err != nil {
		t.Fatalf("buildProxyClient: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://203.0.113.5/x", nil)
	if _, doErr := c.Do(req); doErr != nil {
		t.Fatalf("expected the dial to the unguarded private proxy host to succeed, got %v", doErr)
	}
	if !invoked {
		t.Fatal("expected the victim server handler to be invoked")
	}
}

// ---------------------------------------------------------------------------
// End-to-end: serveStream honours the "proxy" SSRF guard
// ---------------------------------------------------------------------------

// TestServeStreamRejectsBlockedProxyHost proves a request carrying
// ?proxy=http://127.0.0.1:<port> against a protected handler is rejected
// with 403 before any connection reaches the would-be proxy target — the
// httptest.Server's handler must never be invoked, and the request must not
// silently fall back to fetching opts.Dest directly.
func TestServeStreamRejectsBlockedProxyHost(t *testing.T) {
	invoked := false
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		invoked = true
		w.WriteHeader(http.StatusOK)
	}))
	defer victim.Close()

	h := New(Config{Password: "s"})
	dest := base64.RawURLEncoding.EncodeToString([]byte("http://203.0.113.5/video.ts"))
	reqURL := "/proxy/stream?d=" + dest + "&proxy=" + victim.URL + "&api_password=s"
	r := httptest.NewRequest(http.MethodGet, reqURL, nil)
	w := httptest.NewRecorder()

	h.serveStream(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403, body=%s", w.Code, w.Body.String())
	}
	if invoked {
		t.Fatal("victim httptest.Server handler was invoked; the blocked proxy host was dialed")
	}
}

// TestServeStreamAllowsPrivateProxyHostWhenUnprotected drives the full
// serveStream path with an unprotected handler (no Password/IPACL/Secret) and
// a real local httptest.Server standing in for the upstream proxy. Because
// the handler is unprotected, the private (127.0.0.1) proxy host is allowed,
// the guarded dialer reaches it, and the fake proxy's handler observes the
// proxied request end to end.
func TestServeStreamAllowsPrivateProxyHostWhenUnprotected(t *testing.T) {
	invoked := false
	fakeProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		invoked = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer fakeProxy.Close()

	h := New(Config{})
	dest := base64.RawURLEncoding.EncodeToString([]byte("http://203.0.113.5/video.ts"))
	reqURL := "/proxy/stream?d=" + dest + "&proxy=" + fakeProxy.URL
	r := httptest.NewRequest(http.MethodGet, reqURL, nil)
	w := httptest.NewRecorder()

	h.serveStream(w, r)

	if w.Code == http.StatusForbidden {
		t.Fatalf("unprotected handler should allow a private proxy host, got 403: %s", w.Body.String())
	}
	if !invoked {
		t.Fatal("expected the fake proxy server to receive the proxied request")
	}
}
