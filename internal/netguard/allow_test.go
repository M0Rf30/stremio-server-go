// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package netguard

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestParseAllow(t *testing.T) {
	a, bad := ParseAllow(" 192.168.1.10, 10.0.0.0/24 ,nas.lan, fd00::5, [fd00::6], bad_host!, 300.1.1.1/40 ,")
	if a == nil {
		t.Fatal("nil allowlist")
	}
	if len(bad) != 2 {
		t.Errorf("bad = %v, want 2 entries", bad)
	}
	if got := a.String(); got != "192.168.1.10/32,10.0.0.0/24,fd00::5/128,fd00::6/128,nas.lan" {
		t.Errorf("String() = %q", got)
	}
	if a, _ := ParseAllow(" , "); a != nil {
		t.Error("empty spec should yield nil")
	}
}

func TestAllowContains(t *testing.T) {
	a, _ := ParseAllow("192.168.1.10,10.0.0.0/24,169.254.0.0/16")
	for ip, want := range map[string]bool{
		"192.168.1.10":    true,
		"192.168.1.11":    false,
		"10.0.0.200":      true,
		"10.0.1.1":        false,
		"169.254.1.1":     true,
		"169.254.169.254": false, // cloud metadata is never allowlistable
	} {
		if got := a.Contains(net.ParseIP(ip)); got != want {
			t.Errorf("Contains(%s) = %v, want %v", ip, got, want)
		}
	}
	var nilAllow *Allow
	if nilAllow.Contains(net.ParseIP("127.0.0.1")) {
		t.Error("nil allowlist must contain nothing")
	}
}

func TestAllowHostnameResolution(t *testing.T) {
	a, _ := ParseAllow("nas.lan")
	calls := 0
	answer := []net.IPAddr{{IP: net.ParseIP("192.168.1.50")}}
	var fail bool
	a.lookup = func(_ context.Context, host string) ([]net.IPAddr, error) {
		calls++
		if host != "nas.lan" {
			t.Errorf("lookup(%q)", host)
		}
		if fail {
			return nil, errors.New("dns down")
		}
		return answer, nil
	}
	if !a.Contains(net.ParseIP("192.168.1.50")) || a.Contains(net.ParseIP("192.168.1.51")) {
		t.Fatal("hostname not resolved to its address")
	}
	_ = a.Contains(net.ParseIP("192.168.1.50"))
	if calls != 1 {
		t.Errorf("lookup calls = %d, want 1 (cached)", calls)
	}
	// Stale cache: the address moved.
	a.resolved = time.Now().Add(-2 * allowHostRefresh)
	answer = []net.IPAddr{{IP: net.ParseIP("192.168.1.77")}}
	if !a.Contains(net.ParseIP("192.168.1.77")) || a.Contains(net.ParseIP("192.168.1.50")) {
		t.Error("re-resolution did not pick up the new address")
	}
	// A failed refresh keeps the last good answer.
	a.resolved = time.Now().Add(-2 * allowHostRefresh)
	fail = true
	if !a.Contains(net.ParseIP("192.168.1.77")) {
		t.Error("failed lookup dropped the previous addresses")
	}
}

func TestDialControlAllow(t *testing.T) {
	a, _ := ParseAllow("127.0.0.1,192.168.0.0/16")
	ctl := DialControlAllow(true, a)
	for addr, wantOK := range map[string]bool{
		"127.0.0.1:80":       true,  // allowlisted
		"192.168.5.5:443":    true,  // allowlisted range
		"10.1.1.1:80":        false, // private, not listed
		"169.254.169.254:80": false, // metadata, always blocked
		"8.8.8.8:53":         true,  // public
	} {
		err := ctl("tcp", addr, nil)
		if (err == nil) != wantOK {
			t.Errorf("DialControlAllow(%s) err = %v, want ok=%v", addr, err, wantOK)
		}
	}
	// nil allowlist behaves exactly like DialControl.
	if err := DialControlAllow(true, nil)("tcp", "127.0.0.1:80", nil); err == nil {
		t.Error("nil allowlist allowed loopback")
	}
	if err := ValidateIPAllow(net.ParseIP("10.1.1.1"), false, nil); err != nil {
		t.Errorf("blockPrivate=false must allow private: %v", err)
	}
}
