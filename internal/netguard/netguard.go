// Package netguard provides SSRF protection primitives shared across the
// outbound-fetch paths (proxy, /create blob, ftpstream HTTP). It exposes both a
// URL/host pre-flight check and a dialer Control hook that re-validates the
// post-resolution IP at connect time, closing the classic validate-then-dial
// DNS-rebinding TOCTOU gap (the dialer's own resolution is the one actually used).
package netguard

import (
	"fmt"
	"net"
	"syscall"
)

// privateRanges are the loopback, RFC 1918, CGNAT, link-local, and ULA networks
// that an SSRF guard blocks when private addresses are disallowed.
var privateRanges = func() []*net.IPNet {
	cidrs := []string{
		"0.0.0.0/8",      // "this host" / unspecified
		"10.0.0.0/8",     // RFC 1918
		"100.64.0.0/10",  // RFC 6598 CGNAT
		"127.0.0.0/8",    // loopback
		"169.254.0.0/16", // link-local
		"172.16.0.0/12",  // RFC 1918
		"192.168.0.0/16", // RFC 1918
		"::1/128",        // IPv6 loopback
		"fc00::/7",       // IPv6 ULA
		"fe80::/10",      // IPv6 link-local
		"::/128",         // IPv6 unspecified
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}()

// IsPrivate reports whether ip is loopback, RFC 1918, CGNAT, link-local, or ULA.
func IsPrivate(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	for _, r := range privateRanges {
		if r.Contains(ip) {
			return true
		}
	}
	return false
}

// IsCloudMetadata reports whether ip is the well-known cloud-metadata endpoint
// 169.254.169.254 (including its IPv4-mapped IPv6 form).
func IsCloudMetadata(ip net.IP) bool {
	if ip == nil {
		return false
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	return ip4[0] == 169 && ip4[1] == 254 && ip4[2] == 169 && ip4[3] == 254
}

// ValidateIP rejects an IP that an outbound fetch must not reach. The
// cloud-metadata address is always blocked; private/loopback ranges are blocked
// only when blockPrivate is set (callers exposed to untrusted clients).
func ValidateIP(ip net.IP, blockPrivate bool) error {
	if IsCloudMetadata(ip) {
		return fmt.Errorf("blocked cloud-metadata address %s", ip)
	}
	if blockPrivate && IsPrivate(ip) {
		return fmt.Errorf("blocked private address %s", ip)
	}
	return nil
}

// DialControl returns a value for net.Dialer.Control that validates the actual
// resolved IP about to be dialed. Because the dialer invokes this after its own
// DNS resolution and once per candidate address, it forecloses DNS-rebinding:
// the IP checked here is the IP connected to. Returning an error aborts that
// connection attempt.
func DialControl(blockPrivate bool) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("netguard: cannot parse dial address %q: %w", address, err)
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return fmt.Errorf("netguard: dial address %q is not a resolved IP", address)
		}
		return ValidateIP(ip, blockPrivate)
	}
}
