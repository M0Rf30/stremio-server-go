// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package netguard

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"
)

// allowHostRefresh is how long resolved allowlist hostnames are trusted
// before they are looked up again (LAN DHCP addresses can move).
const allowHostRefresh = 5 * time.Minute

// Allow is an operator-configured allowlist of private destinations that
// stay reachable even when private addresses are otherwise blocked. Entries
// are IPs, CIDRs or hostnames; hostnames are resolved and periodically
// re-resolved, and matching is always done on the IP actually dialed, so the
// dial-time check stays DNS-rebinding-safe. Cloud-metadata addresses can
// never be allowed. A nil *Allow allows nothing.
type Allow struct {
	nets  []*net.IPNet
	hosts []string

	mu       sync.Mutex
	hostIPs  []net.IP
	resolved time.Time
	lookup   func(ctx context.Context, host string) ([]net.IPAddr, error)
}

// ParseAllow parses a comma-separated list of IPs, CIDRs and hostnames.
// Invalid entries are returned in bad (the rest still apply). An empty spec
// yields a nil *Allow.
func ParseAllow(spec string) (a *Allow, bad []string) {
	al := &Allow{lookup: net.DefaultResolver.LookupIPAddr}
	for _, e := range strings.Split(spec, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(e); err == nil {
			al.nets = append(al.nets, n)
			continue
		}
		if ip := net.ParseIP(strings.Trim(e, "[]")); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			al.nets = append(al.nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		if validHostname(e) {
			al.hosts = append(al.hosts, strings.ToLower(strings.TrimSuffix(e, ".")))
			continue
		}
		bad = append(bad, e)
	}
	if len(al.nets) == 0 && len(al.hosts) == 0 {
		return nil, bad
	}
	return al, bad
}

func validHostname(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(s, "."), ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if c != '-' && (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
				return false
			}
		}
	}
	return true
}

// Contains reports whether ip is on the allowlist. Cloud-metadata addresses
// are never contained.
func (a *Allow) Contains(ip net.IP) bool {
	if a == nil || ip == nil || IsCloudMetadata(ip) {
		return false
	}
	for _, n := range a.nets {
		if n.Contains(ip) {
			return true
		}
	}
	if len(a.hosts) == 0 {
		return false
	}
	for _, h := range a.hostAddrs() {
		if h.Equal(ip) {
			return true
		}
	}
	return false
}

// hostAddrs returns the cached addresses of the allowlisted hostnames,
// re-resolving them when stale. A failed lookup keeps the previous set.
func (a *Allow) hostAddrs() []net.IP {
	a.mu.Lock()
	defer a.mu.Unlock()
	if time.Since(a.resolved) < allowHostRefresh && a.hostIPs != nil {
		return a.hostIPs
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var ips []net.IP
	ok := false
	for _, h := range a.hosts {
		addrs, err := a.lookup(ctx, h)
		if err != nil {
			continue
		}
		ok = true
		for _, ad := range addrs {
			ips = append(ips, ad.IP)
		}
	}
	if ok || a.hostIPs == nil {
		a.hostIPs = ips
		if a.hostIPs == nil {
			a.hostIPs = []net.IP{}
		}
	}
	a.resolved = time.Now()
	return a.hostIPs
}

// String describes the allowlist for logs.
func (a *Allow) String() string {
	if a == nil {
		return ""
	}
	parts := make([]string, 0, len(a.nets)+len(a.hosts))
	for _, n := range a.nets {
		parts = append(parts, n.String())
	}
	return strings.Join(append(parts, a.hosts...), ",")
}

// ValidateIPAllow is ValidateIP with an allowlist: an allowlisted private
// address passes even when blockPrivate is set. Cloud-metadata is always
// rejected.
func ValidateIPAllow(ip net.IP, blockPrivate bool, a *Allow) error {
	if blockPrivate && a.Contains(ip) {
		return nil
	}
	return ValidateIP(ip, blockPrivate)
}

// DialControlAllow is DialControl with an allowlist (see ValidateIPAllow).
func DialControlAllow(blockPrivate bool, a *Allow) func(network, address string, c syscall.RawConn) error {
	if a == nil {
		return DialControl(blockPrivate)
	}
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("netguard: cannot parse dial address %q: %w", address, err)
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return fmt.Errorf("netguard: dial address %q is not a resolved IP", address)
		}
		return ValidateIPAllow(ip, blockPrivate, a)
	}
}
