package app

import (
	"net"
	"strconv"
	"testing"
)

// TestIsWildcardBindHost checks the classification used to decide whether the
// "unauthenticated, reachable from every interface" startup warning fires.
func TestIsWildcardBindHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"", true},
		{"0.0.0.0", true},
		{"::", true},
		{"127.0.0.1", false},
		{"::1", false},
		{"192.168.1.5", false},
		{"2001:db8::1", false},
	}
	for _, c := range cases {
		if got := isWildcardBindHost(c.host); got != c.want {
			t.Errorf("isWildcardBindHost(%q) = %v; want %v", c.host, got, c.want)
		}
	}
}

// TestBindAddrJoinHostPort verifies the exact net.JoinHostPort semantics the
// BIND_ADDRESS knob relies on: empty preserves today's all-interfaces
// behaviour byte-for-byte, a plain IPv4/hostname yields "host:port", and a
// bare IPv6 literal gets bracketed automatically.
func TestBindAddrJoinHostPort(t *testing.T) {
	const port = 11470
	cases := []struct {
		bindAddr string
		want     string
	}{
		{"", ":11470"}, // default: unchanged from before this knob existed
		{"127.0.0.1", "127.0.0.1:11470"},
		{"::1", "[::1]:11470"},
		{"0.0.0.0", "0.0.0.0:11470"},
		{"::", "[::]:11470"},
	}
	for _, c := range cases {
		got := net.JoinHostPort(c.bindAddr, strconv.Itoa(port))
		if got != c.want {
			t.Errorf("JoinHostPort(%q, %d) = %q; want %q", c.bindAddr, port, got, c.want)
		}
	}
}
