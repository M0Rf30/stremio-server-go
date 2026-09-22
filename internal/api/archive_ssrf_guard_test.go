// Package api — regression tests for SEC-3 (review 2026-09-22): archiveDownload
// and nzb.go's NZB-XML fetch used getClient (api.go), whose dialer only blocks
// the cloud-metadata address and whose default redirect handling follows any
// 30x — so a public URL that redirected to 127.0.0.1/RFC1918, or that
// resolved to a mixed [public, private] DNS answer, could still reach an
// internal host. archiveFetchClient (dialer Control: archiveDialControl,
// CheckRedirect: archiveCheckRedirect) closes both gaps; validateFetchHost's
// anyAddrAllowed helper documents the known pre-flight-only limitation the
// dial-time guard exists to cover.
//
// A real httptest.Server cannot stand in for "a public host that redirects to
// loopback" here: httptest.Server itself listens on 127.0.0.1, so the guarded
// dialer would refuse the very first connection before any redirect is ever
// issued. These tests instead exercise archiveDialControl and
// archiveCheckRedirect directly against a loopback target/URL.
package api

import (
	"net"
	"net/http"
	"testing"
)

// TestArchiveDialControl_BlocksLoopbackByDefault proves archiveFetchClient's
// dialer refuses a loopback target when STREMIO_ARCHIVE_ALLOW_PRIVATE is
// unset — the default, secure posture.
func TestArchiveDialControl_BlocksLoopbackByDefault(t *testing.T) {
	t.Setenv("STREMIO_ARCHIVE_ALLOW_PRIVATE", "")
	if err := archiveDialControl("tcp", "127.0.0.1:8080", nil); err == nil {
		t.Error("archiveDialControl(loopback) = nil, want error")
	}
}

// TestArchiveDialControl_AllowsLoopbackWhenOptedIn proves the opt-in env var
// relaxes the same dial-time guard, and that archiveDialControl re-reads the
// env var per call rather than baking the decision in at package-init time.
func TestArchiveDialControl_AllowsLoopbackWhenOptedIn(t *testing.T) {
	t.Setenv("STREMIO_ARCHIVE_ALLOW_PRIVATE", "1")
	if err := archiveDialControl("tcp", "127.0.0.1:8080", nil); err != nil {
		t.Errorf("archiveDialControl(loopback) with opt-in = %v, want nil", err)
	}
}

// TestArchiveDialControl_AlwaysBlocksCloudMetadata proves the cloud-metadata
// address is refused even when private hosts are opted in — matching
// netguard.ValidateIP's unconditional check.
func TestArchiveDialControl_AlwaysBlocksCloudMetadata(t *testing.T) {
	t.Setenv("STREMIO_ARCHIVE_ALLOW_PRIVATE", "1")
	if err := archiveDialControl("tcp", "169.254.169.254:80", nil); err == nil {
		t.Error("archiveDialControl(cloud-metadata) with opt-in = nil, want error")
	}
}

// TestArchiveCheckRedirect_BlocksRedirectToLoopback simulates the "allowed
// public host redirects to a private target" finding directly: a public URL
// that 30x-redirects to 127.0.0.1 must be refused by CheckRedirect before the
// client ever issues the second request.
func TestArchiveCheckRedirect_BlocksRedirectToLoopback(t *testing.T) {
	t.Setenv("STREMIO_ARCHIVE_ALLOW_PRIVATE", "")
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:9/internal", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := archiveCheckRedirect(req, nil); err == nil {
		t.Error("archiveCheckRedirect(loopback target) = nil, want error")
	}
}

// TestArchiveCheckRedirect_AllowsPublicTarget proves legitimate redirects
// (e.g. CDN 301s) between public hosts are still permitted.
func TestArchiveCheckRedirect_AllowsPublicTarget(t *testing.T) {
	t.Setenv("STREMIO_ARCHIVE_ALLOW_PRIVATE", "")
	req, err := http.NewRequest(http.MethodGet, "http://1.1.1.1/file.zip", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := archiveCheckRedirect(req, nil); err != nil {
		t.Errorf("archiveCheckRedirect(public target) = %v, want nil", err)
	}
}

// TestArchiveCheckRedirect_StopsAfterMaxRedirects proves the redirect chain
// is bounded, independent of host validation.
func TestArchiveCheckRedirect_StopsAfterMaxRedirects(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://1.1.1.1/file.zip", nil)
	if err != nil {
		t.Fatal(err)
	}
	via := make([]*http.Request, archiveMaxRedirects)
	if err := archiveCheckRedirect(req, via); err == nil {
		t.Error("archiveCheckRedirect at the redirect cap = nil, want error")
	}
}

// TestAnyAddrAllowed exercises the mixed-DNS-answer helper factored out of
// validateFetchHost with literal []net.IP inputs (SEC-3: "validateFetchHost
// is bypassed by ... a mixed DNS answer").
func TestAnyAddrAllowed(t *testing.T) {
	tests := []struct {
		name  string
		addrs []net.IP
		want  bool
	}{
		{
			name:  "all private",
			addrs: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.1")},
			want:  false,
		},
		{
			name:  "mixed public and private",
			addrs: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("1.1.1.1")},
			want:  true,
		},
		{
			name:  "all public",
			addrs: []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("1.1.1.1")},
			want:  true,
		},
		{
			name:  "cloud metadata only",
			addrs: []net.IP{net.ParseIP("169.254.169.254")},
			want:  false,
		},
		{
			name:  "empty",
			addrs: nil,
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := anyAddrAllowed(tt.addrs); got != tt.want {
				t.Errorf("anyAddrAllowed(%v) = %v, want %v", tt.addrs, got, tt.want)
			}
		})
	}
}
