// Package api — regression tests for the SSRF pre-flight (validateFetchHost)
// and local-path allowlist confinement (archiveResolveLocalPath) added to
// archive.go in response to the security review findings:
//
//   - /{zip,rar,7zip,tar,tgz}/create and /nzb/create previously reached
//     private/loopback addresses because getClient (api.go) only blocks the
//     cloud-metadata address; validateFetchHost closes that gap.
//   - /{zip,rar,7zip,tar,tgz}/create previously treated any non-http(s)
//     "url" as an arbitrary local filesystem path with no root confinement;
//     archiveResolveLocalPath closes that gap.
package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── validateFetchHost (SSRF pre-flight) ───────────────────────────────────

// TestValidateFetchHost_BlocksPrivateByDefault verifies the default (secure)
// posture: a literal loopback/RFC1918 target is rejected without any network
// I/O, and the cloud-metadata address is rejected too.
func TestValidateFetchHost_BlocksPrivateByDefault(t *testing.T) {
	for _, u := range []string{
		"http://127.0.0.1:8080/admin",
		"http://192.168.1.1/",
		"http://169.254.169.254/latest/meta-data/",
	} {
		if err := validateFetchHost(u); err == nil {
			t.Errorf("validateFetchHost(%q) = nil; want error", u)
		} else if !errors.Is(err, errFetchHostNotAllowed) {
			t.Errorf("validateFetchHost(%q) = %v; want errFetchHostNotAllowed", u, err)
		}
	}
}

// TestValidateFetchHost_AllowsPublic verifies a public literal IP target is
// not blocked (no real network I/O occurs — DNS resolution of a literal IP
// is a no-op, and no HTTP request is made by validateFetchHost itself).
func TestValidateFetchHost_AllowsPublic(t *testing.T) {
	if err := validateFetchHost("http://1.1.1.1/file.zip"); err != nil {
		t.Errorf("validateFetchHost(public IP) = %v; want nil", err)
	}
}

// TestValidateFetchHost_AllowPrivateOptIn verifies STREMIO_ARCHIVE_ALLOW_PRIVATE
// restores the ability to target private/loopback hosts, which is a
// supported use case (archives hosted on a LAN NAS or sibling container).
func TestValidateFetchHost_AllowPrivateOptIn(t *testing.T) {
	t.Setenv("STREMIO_ARCHIVE_ALLOW_PRIVATE", "1")
	if err := validateFetchHost("http://127.0.0.1:8080/admin"); err != nil {
		t.Errorf("validateFetchHost with opt-in = %v; want nil", err)
	}
}

// TestArchiveAllowPrivateHostsParsing exercises the accepted truthy/falsy
// spellings of STREMIO_ARCHIVE_ALLOW_PRIVATE.
func TestArchiveAllowPrivateHostsParsing(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"0":     false,
		"off":   false,
		"false": false,
		"1":     true,
		"true":  true,
		"TRUE":  true,
		"on":    true,
	}
	for v, want := range cases {
		t.Setenv("STREMIO_ARCHIVE_ALLOW_PRIVATE", v)
		if got := archiveAllowPrivateHosts(); got != want {
			t.Errorf("archiveAllowPrivateHosts() with env=%q = %v, want %v", v, got, want)
		}
	}
}

// ─── archiveResolveLocalPath (local-path allowlist confinement) ───────────

// TestArchiveResolveLocalPath_DisabledByDefault verifies local-path archives
// are rejected outright when neither STREMIO_ARCHIVE_LOCAL_ROOT nor
// LOCAL_FILES_DIR is set.
func TestArchiveResolveLocalPath_DisabledByDefault(t *testing.T) {
	t.Setenv("STREMIO_ARCHIVE_LOCAL_ROOT", "")
	t.Setenv("LOCAL_FILES_DIR", "")
	if _, err := archiveResolveLocalPath("/etc/passwd"); err == nil {
		t.Error("expected error when no local root is configured, got nil")
	}
}

// TestArchiveResolveLocalPath_WithinRoot verifies a path inside the
// configured root resolves without error.
func TestArchiveResolveLocalPath_WithinRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("STREMIO_ARCHIVE_LOCAL_ROOT", root)
	t.Setenv("LOCAL_FILES_DIR", "")

	target := filepath.Join(root, "movie.zip")
	if err := os.WriteFile(target, []byte("zip"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	resolved, err := archiveResolveLocalPath(target)
	if err != nil {
		t.Fatalf("archiveResolveLocalPath(%q) = %v; want nil", target, err)
	}
	if resolved == "" {
		t.Error("resolved path is empty")
	}
}

// TestArchiveResolveLocalPath_LocalFilesDirFallback verifies LOCAL_FILES_DIR
// (the existing local-files-addon convention) is used as the root when
// STREMIO_ARCHIVE_LOCAL_ROOT is unset.
func TestArchiveResolveLocalPath_LocalFilesDirFallback(t *testing.T) {
	root := t.TempDir()
	t.Setenv("STREMIO_ARCHIVE_LOCAL_ROOT", "")
	t.Setenv("LOCAL_FILES_DIR", root)

	target := filepath.Join(root, "movie.zip")
	if _, err := archiveResolveLocalPath(target); err != nil {
		t.Errorf("archiveResolveLocalPath via LOCAL_FILES_DIR fallback = %v; want nil", err)
	}
}

// TestArchiveResolveLocalPath_TraversalEscape verifies a "../" traversal
// attempt that resolves outside the configured root is rejected.
func TestArchiveResolveLocalPath_TraversalEscape(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "allowed")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	secret := filepath.Join(parent, "secret.zip")
	if err := os.WriteFile(secret, []byte("zip"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("STREMIO_ARCHIVE_LOCAL_ROOT", root)
	t.Setenv("LOCAL_FILES_DIR", "")

	escape := filepath.Join(root, "..", "secret.zip")
	if _, err := archiveResolveLocalPath(escape); err == nil {
		t.Errorf("archiveResolveLocalPath(%q) = nil; want error (escapes root)", escape)
	}
}

// TestArchiveResolveLocalPath_SymlinkEscape verifies a symlink inside the
// allowed root that points outside it is rejected — filepath.EvalSymlinks
// resolves the real target before the root-containment check runs, so a
// naive prefix check on the un-resolved path cannot be bypassed this way.
func TestArchiveResolveLocalPath_SymlinkEscape(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "allowed")
	outside := filepath.Join(parent, "outside")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("Mkdir root: %v", err)
	}
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatalf("Mkdir outside: %v", err)
	}
	secret := filepath.Join(outside, "secret.zip")
	if err := os.WriteFile(secret, []byte("zip"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	t.Setenv("STREMIO_ARCHIVE_LOCAL_ROOT", root)
	t.Setenv("LOCAL_FILES_DIR", "")

	via := filepath.Join(link, "secret.zip")
	if _, err := archiveResolveLocalPath(via); err == nil {
		t.Errorf("archiveResolveLocalPath(%q) = nil; want error (symlink escapes root)", via)
	}
}

// ─── archiveHandleCreate: uniform local-path rejection (filesystem-existence
// oracle closure) ───────────────────────────────────────────────────────────

// archiveCreateLocalReq POSTs {"url": path} to /zip/create and returns the
// recorded response.
func archiveCreateLocalReq(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"url":%q}`, path)
	req := httptest.NewRequest(http.MethodPost, "/zip/create", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestHandlerArchiveCreate_LocalPathUniformRejection verifies that a local
// "url" outside the configured allowlist root and a local "url" that is
// inside the root but does not exist produce byte-identical status+body —
// closing the filesystem-existence oracle described in the review finding
// (distinct error strings previously let a caller probe which paths exist).
func TestHandlerArchiveCreate_LocalPathUniformRejection(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "allowed")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	t.Setenv("STREMIO_ARCHIVE_LOCAL_ROOT", root)
	t.Setenv("LOCAL_FILES_DIR", "")

	outsidePath := filepath.Join(parent, "definitely-exists-but-outside-root.zip")
	if err := os.WriteFile(outsidePath, []byte("zip"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	nonexistentInsideRoot := filepath.Join(root, "does-not-exist.zip")

	h := newHandler(t)
	outsideRec := archiveCreateLocalReq(t, h, outsidePath)
	nonexistentRec := archiveCreateLocalReq(t, h, nonexistentInsideRoot)

	if outsideRec.Code != http.StatusBadRequest {
		t.Errorf("outside-root status = %d; want %d", outsideRec.Code, http.StatusBadRequest)
	}
	if outsideRec.Code != nonexistentRec.Code {
		t.Errorf("status mismatch: outside-root=%d nonexistent=%d; want identical",
			outsideRec.Code, nonexistentRec.Code)
	}
	if outsideRec.Body.String() != nonexistentRec.Body.String() {
		t.Errorf("body mismatch: outside-root=%q nonexistent=%q; want identical",
			outsideRec.Body.String(), nonexistentRec.Body.String())
	}
}

// TestHandlerArchiveCreate_LocalPathDisabledByDefault verifies that with no
// allowlist root configured, a local "url" pointing at a real file is
// rejected — the finding's core repro ({"url":"/home/<user>/Downloads/foo.zip"})
// no longer succeeds by default.
func TestHandlerArchiveCreate_LocalPathDisabledByDefault(t *testing.T) {
	t.Setenv("STREMIO_ARCHIVE_LOCAL_ROOT", "")
	t.Setenv("LOCAL_FILES_DIR", "")

	real := filepath.Join(t.TempDir(), "real.zip")
	if err := os.WriteFile(real, []byte("zip"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	h := newHandler(t)
	rec := archiveCreateLocalReq(t, h, real)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if rec.Body.String() != archiveLocalPathErrMsg+"\n" {
		t.Errorf("body = %q; want %q", rec.Body.String(), archiveLocalPathErrMsg+"\n")
	}
}

// TestHandlerArchiveCreate_BlocksPrivateURLByDefault verifies /zip/create
// rejects a private-address "url" by default and never invokes the local
// handler — closing the SSRF finding for the archive-download path.
func TestHandlerArchiveCreate_BlocksPrivateURLByDefault(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newHandler(t)
	body := fmt.Sprintf(`{"url":"%s/test.zip"}`, srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/zip/create", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d; want %d; body = %s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if called {
		t.Error("local handler was invoked; the download should have been rejected before connecting")
	}
}
