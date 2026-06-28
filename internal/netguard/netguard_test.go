package netguard

import (
	"net"
	"strings"
	"testing"
)

func TestIsCloudMetadata(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"169.254.169.254":        true,
		"::ffff:169.254.169.254": true, // IPv4-mapped form
		"169.254.169.253":        false,
		"169.254.0.1":            false,
		"10.0.0.1":               false,
		"8.8.8.8":                false,
	}
	for s, want := range cases {
		if got := IsCloudMetadata(net.ParseIP(s)); got != want {
			t.Errorf("IsCloudMetadata(%s) = %v, want %v", s, got, want)
		}
	}
	if IsCloudMetadata(nil) {
		t.Error("IsCloudMetadata(nil) = true, want false")
	}
}

func TestIsPrivate(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"127.0.0.1":     true,
		"10.1.2.3":      true,
		"172.16.0.1":    true,
		"172.31.255.1":  true,
		"172.32.0.1":    false, // just outside RFC 1918
		"192.168.1.1":   true,
		"100.64.0.1":    true, // CGNAT
		"169.254.1.1":   true, // link-local
		"::1":           true,
		"fc00::1":       true,
		"fe80::1":       true,
		"8.8.8.8":       false,
		"1.1.1.1":       false,
		"93.184.216.34": false,
	}
	for s, want := range cases {
		if got := IsPrivate(net.ParseIP(s)); got != want {
			t.Errorf("IsPrivate(%s) = %v, want %v", s, got, want)
		}
	}
	if IsPrivate(nil) {
		t.Error("IsPrivate(nil) = true, want false")
	}
}

func TestValidateIP(t *testing.T) {
	t.Parallel()
	// Cloud metadata is always blocked, regardless of blockPrivate.
	if err := ValidateIP(net.ParseIP("169.254.169.254"), false); err == nil {
		t.Error("ValidateIP(metadata, blockPrivate=false) = nil, want error")
	}
	if err := ValidateIP(net.ParseIP("169.254.169.254"), true); err == nil {
		t.Error("ValidateIP(metadata, blockPrivate=true) = nil, want error")
	}
	// Private blocked only when blockPrivate is set.
	if err := ValidateIP(net.ParseIP("127.0.0.1"), true); err == nil {
		t.Error("ValidateIP(loopback, blockPrivate=true) = nil, want error")
	}
	if err := ValidateIP(net.ParseIP("127.0.0.1"), false); err != nil {
		t.Errorf("ValidateIP(loopback, blockPrivate=false) = %v, want nil", err)
	}
	// Public always allowed.
	if err := ValidateIP(net.ParseIP("8.8.8.8"), true); err != nil {
		t.Errorf("ValidateIP(public, blockPrivate=true) = %v, want nil", err)
	}
}

func TestDialControl(t *testing.T) {
	t.Parallel()
	block := DialControl(true)

	// Resolved private address is rejected at dial time.
	if err := block("tcp", "127.0.0.1:443", nil); err == nil {
		t.Error("DialControl(true) on 127.0.0.1 = nil, want error")
	}
	// Cloud metadata rejected even when blockPrivate is false.
	allowPrivate := DialControl(false)
	if err := allowPrivate("tcp", "169.254.169.254:80", nil); err == nil {
		t.Error("DialControl(false) on metadata = nil, want error")
	}
	// Private allowed when blockPrivate is false (localhost-trust default).
	if err := allowPrivate("tcp", "192.168.1.10:21", nil); err != nil {
		t.Errorf("DialControl(false) on private = %v, want nil", err)
	}
	// Public allowed.
	if err := block("tcp", "8.8.8.8:443", nil); err != nil {
		t.Errorf("DialControl(true) on public = %v, want nil", err)
	}
	// A non-IP / malformed address is rejected (Control receives resolved IPs).
	if err := block("tcp", "not-an-address", nil); err == nil {
		t.Error("DialControl on malformed address = nil, want error")
	}
	if err := block("tcp", "example.com:443", nil); err == nil {
		t.Error("DialControl on unresolved host = nil, want error")
	} else if !strings.Contains(err.Error(), "resolved IP") {
		t.Errorf("unexpected error for unresolved host: %v", err)
	}
}
