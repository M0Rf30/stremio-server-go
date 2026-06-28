package streamproxy

import (
	"errors"
	"net"
	"net/http/httptest"
	"testing"
	"time"
)

// testSecret is a valid 32-byte AES-256 key used across auth tests.
var testSecret = []byte("abcdefghijklmnopqrstuvwxyz012345")

// altSecret is a different 32-byte key to verify cross-key rejection.
var altSecret = []byte("zyxwvutsrqponmlkjihgfedcba987654")

// authNewHandler builds a Handler for auth tests.
func authNewHandler(cfg Config) *Handler { return New(cfg) }

// ---------------------------------------------------------------------------
// signToken / verifyToken round-trip
// ---------------------------------------------------------------------------

func TestAuthSignVerifyRoundTrip(t *testing.T) {
	h := authNewHandler(Config{Secret: testSecret})
	tok := token{
		Endpoint: "/proxy/stream",
		Params:   map[string]string{"quality": "high"},
		Exp:      time.Now().Add(time.Hour).Unix(),
		IP:       "",
	}
	signed, err := h.signToken(tok)
	if err != nil {
		t.Fatalf("signToken: %v", err)
	}
	got, err := h.verifyToken(signed, nil)
	if err != nil {
		t.Fatalf("verifyToken: %v", err)
	}
	if got.Endpoint != tok.Endpoint {
		t.Errorf("endpoint: got %q want %q", got.Endpoint, tok.Endpoint)
	}
	if got.Params["quality"] != "high" {
		t.Errorf("params mismatch: got %v", got.Params)
	}
}

func TestAuthSignNoSecret(t *testing.T) {
	h := authNewHandler(Config{})
	_, err := h.signToken(token{Exp: time.Now().Add(time.Hour).Unix()})
	if err == nil {
		t.Fatal("expected error without secret, got nil")
	}
}

func TestAuthVerifyNoSecret(t *testing.T) {
	h := authNewHandler(Config{})
	_, err := h.verifyToken("anytoken", nil)
	if err == nil {
		t.Fatal("expected error without secret, got nil")
	}
}

func TestAuthVerifyExpired(t *testing.T) {
	h := authNewHandler(Config{Secret: testSecret})
	tok := token{Exp: time.Now().Add(-time.Second).Unix()} // already past
	signed, err := h.signToken(tok)
	if err != nil {
		t.Fatalf("signToken: %v", err)
	}
	_, err = h.verifyToken(signed, nil)
	if err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
}

func TestAuthVerifyTampered(t *testing.T) {
	h := authNewHandler(Config{Secret: testSecret})
	tok := token{Exp: time.Now().Add(time.Hour).Unix()}
	signed, err := h.signToken(tok)
	if err != nil {
		t.Fatalf("signToken: %v", err)
	}
	// Flip a byte inside the ciphertext (not the nonce prefix).
	bs := []byte(signed)
	bs[len(bs)-1] ^= 0x01
	_, err = h.verifyToken(string(bs), nil)
	if err == nil {
		t.Fatal("expected error for tampered token, got nil")
	}
}

func TestAuthVerifyWrongSecret(t *testing.T) {
	h1 := authNewHandler(Config{Secret: testSecret})
	h2 := authNewHandler(Config{Secret: altSecret})
	tok := token{Exp: time.Now().Add(time.Hour).Unix()}
	signed, err := h1.signToken(tok)
	if err != nil {
		t.Fatalf("signToken: %v", err)
	}
	_, err = h2.verifyToken(signed, nil)
	if err == nil {
		t.Fatal("expected error verifying with wrong secret, got nil")
	}
}

func TestAuthVerifyBadBase64(t *testing.T) {
	h := authNewHandler(Config{Secret: testSecret})
	_, err := h.verifyToken("!!!notbase64!!!", nil)
	if err == nil {
		t.Fatal("expected error for non-base64 token")
	}
}

func TestAuthVerifyTokenTooShort(t *testing.T) {
	h := authNewHandler(Config{Secret: testSecret})
	// A valid base64url string that decodes to fewer bytes than a GCM nonce.
	_, err := h.verifyToken("YQ", nil) // decodes to "a", 1 byte < 12-byte nonce
	if err == nil {
		t.Fatal("expected error for token too short")
	}
}

// ---------------------------------------------------------------------------
// IP-bound token
// ---------------------------------------------------------------------------

func TestAuthVerifyIPBoundMatch(t *testing.T) {
	h := authNewHandler(Config{Secret: testSecret})
	tok := token{
		Exp: time.Now().Add(time.Hour).Unix(),
		IP:  "1.2.3.4",
	}
	signed, err := h.signToken(tok)
	if err != nil {
		t.Fatalf("signToken: %v", err)
	}
	if _, err := h.verifyToken(signed, net.ParseIP("1.2.3.4")); err != nil {
		t.Errorf("expected success for matching IP, got %v", err)
	}
}

func TestAuthVerifyIPBoundMismatch(t *testing.T) {
	h := authNewHandler(Config{Secret: testSecret})
	tok := token{
		Exp: time.Now().Add(time.Hour).Unix(),
		IP:  "1.2.3.4",
	}
	signed, err := h.signToken(tok)
	if err != nil {
		t.Fatalf("signToken: %v", err)
	}
	_, err = h.verifyToken(signed, net.ParseIP("9.9.9.9"))
	if err == nil {
		t.Error("expected error for mismatched IP, got nil")
	}
}

func TestAuthVerifyIPBoundNilClient(t *testing.T) {
	h := authNewHandler(Config{Secret: testSecret})
	tok := token{
		Exp: time.Now().Add(time.Hour).Unix(),
		IP:  "1.2.3.4",
	}
	signed, err := h.signToken(tok)
	if err != nil {
		t.Fatalf("signToken: %v", err)
	}
	// nil client → empty string, won't match "1.2.3.4"
	_, err = h.verifyToken(signed, nil)
	if err == nil {
		t.Error("expected error when client IP is nil and token has IP bound")
	}
}

// ---------------------------------------------------------------------------
// authorize — no auth configured
// ---------------------------------------------------------------------------

func TestAuthorizeNoAuth(t *testing.T) {
	h := authNewHandler(Config{})
	r := httptest.NewRequest("GET", "/proxy/stream", nil)
	r.RemoteAddr = "203.0.113.1:1234"
	if err := h.authorize(r); err != nil {
		t.Errorf("expected nil for no-auth config, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// authorize — password check
// ---------------------------------------------------------------------------

func TestAuthorizePasswordCorrect(t *testing.T) {
	h := authNewHandler(Config{Password: "secret123"})
	r := httptest.NewRequest("GET", "/?api_password=secret123", nil)
	r.RemoteAddr = "203.0.113.1:1234"
	if err := h.authorize(r); err != nil {
		t.Errorf("correct password: got %v", err)
	}
}

func TestAuthorizePasswordWrong(t *testing.T) {
	h := authNewHandler(Config{Password: "secret123"})
	r := httptest.NewRequest("GET", "/?api_password=wrong", nil)
	r.RemoteAddr = "203.0.113.1:1234"
	if err := h.authorize(r); !errors.Is(err, errUnauthorized) {
		t.Errorf("wrong password: got %v want errUnauthorized", err)
	}
}

func TestAuthorizePasswordMissing(t *testing.T) {
	h := authNewHandler(Config{Password: "secret123"})
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.1:1234"
	if err := h.authorize(r); !errors.Is(err, errUnauthorized) {
		t.Errorf("missing password: got %v want errUnauthorized", err)
	}
}

// ---------------------------------------------------------------------------
// authorize — IP ACL
// ---------------------------------------------------------------------------

func TestAuthorizeIPACLAllowed(t *testing.T) {
	_, cidr, _ := net.ParseCIDR("10.0.0.0/8")
	h := authNewHandler(Config{IPACL: []*net.IPNet{cidr}})
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.1.2.3:5000"
	if err := h.authorize(r); err != nil {
		t.Errorf("IP in allowed CIDR: got %v want nil", err)
	}
}

func TestAuthorizeIPACLDenied(t *testing.T) {
	_, cidr, _ := net.ParseCIDR("10.0.0.0/8")
	h := authNewHandler(Config{IPACL: []*net.IPNet{cidr}})
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.5:5000"
	if err := h.authorize(r); !errors.Is(err, errForbidden) {
		t.Errorf("IP outside CIDR: got %v want errForbidden", err)
	}
}

func TestAuthorizeIPACLEmpty(t *testing.T) {
	h := authNewHandler(Config{IPACL: []*net.IPNet{}})
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.5:5000"
	if err := h.authorize(r); err != nil {
		t.Errorf("empty IPACL should allow all: got %v", err)
	}
}

func TestAuthorizeIPACLMultiCIDR(t *testing.T) {
	_, cidr1, _ := net.ParseCIDR("10.0.0.0/8")
	_, cidr2, _ := net.ParseCIDR("192.168.0.0/16")
	h := authNewHandler(Config{IPACL: []*net.IPNet{cidr1, cidr2}})
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.168.1.1:5000"
	if err := h.authorize(r); err != nil {
		t.Errorf("IP in second CIDR: got %v want nil", err)
	}
}

func TestAuthorizeIPACLExactHost(t *testing.T) {
	_, cidr, _ := net.ParseCIDR("203.0.113.7/32")
	h := authNewHandler(Config{IPACL: []*net.IPNet{cidr}})

	t.Run("match", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "203.0.113.7:1234"
		if err := h.authorize(r); err != nil {
			t.Errorf("exact match: got %v want nil", err)
		}
	})
	t.Run("no-match", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "203.0.113.8:1234"
		if err := h.authorize(r); !errors.Is(err, errForbidden) {
			t.Errorf("outside exact CIDR: got %v want errForbidden", err)
		}
	})
}

// ---------------------------------------------------------------------------
// authorize — token path
// ---------------------------------------------------------------------------

func TestAuthorizeTokenValidBypassesPassword(t *testing.T) {
	h := authNewHandler(Config{Secret: testSecret, Password: "required"})
	tok := token{
		Endpoint: "/proxy/stream",
		Exp:      time.Now().Add(time.Hour).Unix(),
	}
	signed, err := h.signToken(tok)
	if err != nil {
		t.Fatalf("signToken: %v", err)
	}
	r := httptest.NewRequest("GET", "/proxy/stream?token="+signed, nil)
	r.RemoteAddr = "203.0.113.1:1234"
	if err := h.authorize(r); err != nil {
		t.Errorf("valid token should bypass password: got %v", err)
	}
}

func TestAuthorizeTokenEndpointMismatch(t *testing.T) {
	h := authNewHandler(Config{Secret: testSecret})
	tok := token{
		Endpoint: "/proxy/stream",
		Exp:      time.Now().Add(time.Hour).Unix(),
	}
	signed, err := h.signToken(tok)
	if err != nil {
		t.Fatalf("signToken: %v", err)
	}
	r := httptest.NewRequest("GET", "/proxy/hls/manifest.m3u8?token="+signed, nil)
	r.RemoteAddr = "203.0.113.1:1234"
	if err := h.authorize(r); !errors.Is(err, errUnauthorized) {
		t.Errorf("endpoint mismatch: got %v want errUnauthorized", err)
	}
}

func TestAuthorizeTokenEmptyEndpointSkipsCheck(t *testing.T) {
	// An empty Endpoint field in the token skips the path check (legacy tokens).
	h := authNewHandler(Config{Secret: testSecret})
	tok := token{
		Endpoint: "", // no binding
		Exp:      time.Now().Add(time.Hour).Unix(),
	}
	signed, err := h.signToken(tok)
	if err != nil {
		t.Fatalf("signToken: %v", err)
	}
	r := httptest.NewRequest("GET", "/any/path?token="+signed, nil)
	r.RemoteAddr = "203.0.113.1:1234"
	if err := h.authorize(r); err != nil {
		t.Errorf("empty endpoint: got %v want nil", err)
	}
}

func TestAuthorizeTokenBadToken(t *testing.T) {
	h := authNewHandler(Config{Secret: testSecret})
	r := httptest.NewRequest("GET", "/?token=garbage_token", nil)
	r.RemoteAddr = "203.0.113.1:1234"
	if err := h.authorize(r); !errors.Is(err, errUnauthorized) {
		t.Errorf("bad token: got %v want errUnauthorized", err)
	}
}

// Token present but no Secret configured → falls through to password check.
func TestAuthorizeTokenPresentNoSecret(t *testing.T) {
	h := authNewHandler(Config{}) // no Secret
	r := httptest.NewRequest("GET", "/?token=whatever", nil)
	r.RemoteAddr = "203.0.113.1:1234"
	// No password either → passes.
	if err := h.authorize(r); err != nil {
		t.Errorf("token with no secret should fall through to no-auth: got %v", err)
	}
}

// ---------------------------------------------------------------------------
// authorize — token Params binding
// ---------------------------------------------------------------------------

func TestAuthorizeTokenParamsMatch(t *testing.T) {
	// Token sealed with Params{"d": "https://a"} must be accepted when the
	// live request carries ?d=https://a.
	h := authNewHandler(Config{Secret: testSecret})
	tok := token{
		Endpoint: "/proxy/stream",
		Params:   map[string]string{"d": "https://a"},
		Exp:      time.Now().Add(time.Hour).Unix(),
	}
	signed, err := h.signToken(tok)
	if err != nil {
		t.Fatalf("signToken: %v", err)
	}
	r := httptest.NewRequest("GET", "/proxy/stream?d=https%3A%2F%2Fa&token="+signed, nil)
	r.RemoteAddr = "203.0.113.1:1234"
	if err := h.authorize(r); err != nil {
		t.Errorf("matching params: got %v want nil", err)
	}
}

func TestAuthorizeTokenParamsMismatch(t *testing.T) {
	// Token sealed with Params{"d": "https://a"} must be rejected when the
	// live request carries ?d=https://b.
	h := authNewHandler(Config{Secret: testSecret})
	tok := token{
		Endpoint: "/proxy/stream",
		Params:   map[string]string{"d": "https://a"},
		Exp:      time.Now().Add(time.Hour).Unix(),
	}
	signed, err := h.signToken(tok)
	if err != nil {
		t.Fatalf("signToken: %v", err)
	}
	r := httptest.NewRequest("GET", "/proxy/stream?d=https%3A%2F%2Fb&token="+signed, nil)
	r.RemoteAddr = "203.0.113.1:1234"
	if err := h.authorize(r); !errors.Is(err, errUnauthorized) {
		t.Errorf("mismatched params: got %v want errUnauthorized", err)
	}
}

func TestAuthorizeTokenParamsMissingKey(t *testing.T) {
	// Token sealed with Params{"d": "https://a"} must be rejected when the
	// live request omits the ?d param entirely.
	h := authNewHandler(Config{Secret: testSecret})
	tok := token{
		Endpoint: "/proxy/stream",
		Params:   map[string]string{"d": "https://a"},
		Exp:      time.Now().Add(time.Hour).Unix(),
	}
	signed, err := h.signToken(tok)
	if err != nil {
		t.Fatalf("signToken: %v", err)
	}
	r := httptest.NewRequest("GET", "/proxy/stream?token="+signed, nil)
	r.RemoteAddr = "203.0.113.1:1234"
	if err := h.authorize(r); !errors.Is(err, errUnauthorized) {
		t.Errorf("missing sealed param: got %v want errUnauthorized", err)
	}
}

func TestAuthorizeTokenEmptyParamsSkipsCheck(t *testing.T) {
	// A token with an empty Params map imposes no query-parameter constraint
	// (legacy tokens must keep working).
	h := authNewHandler(Config{Secret: testSecret})
	tok := token{
		Endpoint: "/proxy/stream",
		Params:   map[string]string{}, // empty → no constraint
		Exp:      time.Now().Add(time.Hour).Unix(),
	}
	signed, err := h.signToken(tok)
	if err != nil {
		t.Fatalf("signToken: %v", err)
	}
	r := httptest.NewRequest("GET", "/proxy/stream?d=anything&token="+signed, nil)
	r.RemoteAddr = "203.0.113.1:1234"
	if err := h.authorize(r); err != nil {
		t.Errorf("empty params: got %v want nil", err)
	}
}

func TestAuthorizeTokenNilParamsSkipsCheck(t *testing.T) {
	// A token with a nil Params map (JSON-omitted) also imposes no constraint.
	h := authNewHandler(Config{Secret: testSecret})
	tok := token{
		Endpoint: "/proxy/stream",
		// Params intentionally zero-value (nil map)
		Exp: time.Now().Add(time.Hour).Unix(),
	}
	signed, err := h.signToken(tok)
	if err != nil {
		t.Fatalf("signToken: %v", err)
	}
	r := httptest.NewRequest("GET", "/proxy/stream?d=anything&token="+signed, nil)
	r.RemoteAddr = "203.0.113.1:1234"
	if err := h.authorize(r); err != nil {
		t.Errorf("nil params: got %v want nil", err)
	}
}

// ---------------------------------------------------------------------------
// clientIP
// ---------------------------------------------------------------------------

func TestClientIPDirect(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.99:12345"
	ip := clientIP(r)
	if ip.String() != "203.0.113.99" {
		t.Errorf("direct IP: got %q want %q", ip, "203.0.113.99")
	}
}

func TestClientIPXFFTrustedFromLoopback(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:8080" // loopback → trust XFF
	r.Header.Set("X-Forwarded-For", "203.0.113.55, 10.0.0.1")
	ip := clientIP(r)
	if ip.String() != "203.0.113.55" {
		t.Errorf("XFF from loopback: got %q want %q", ip, "203.0.113.55")
	}
}

func TestClientIPXFFTrustedFromPrivate(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.168.1.100:8080" // private → trust XFF
	r.Header.Set("X-Forwarded-For", "8.8.8.8")
	ip := clientIP(r)
	if ip.String() != "8.8.8.8" {
		t.Errorf("XFF from private peer: got %q want %q", ip, "8.8.8.8")
	}
}

func TestClientIPXFFIgnoredFromPublic(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.1:8080" // public → XFF NOT trusted
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	ip := clientIP(r)
	if ip.String() != "203.0.113.1" {
		t.Errorf("XFF from public peer ignored: got %q want %q", ip, "203.0.113.1")
	}
}

func TestClientIPNoPort(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.7" // no port component
	ip := clientIP(r)
	if ip == nil || ip.String() != "203.0.113.7" {
		t.Errorf("no-port RemoteAddr: got %v want 203.0.113.7", ip)
	}
}
