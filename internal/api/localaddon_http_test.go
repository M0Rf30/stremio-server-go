// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/types"
)

func TestLocalFileStreamURL(t *testing.T) {
	s := &server{
		cfg: types.Config{
			LocalFilesPublicURL: "http://192.168.1.50:11470",
		},
	}

	m := localMeta{
		LocalHex: "abc123",
		Path:     "/media/Test Episode.mkv",
	}

	got := s.localFileStreamURL(m)
	want := "http://192.168.1.50:11470/local-addon/file/abc123"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLocalFileStreamURLFallsBackToFileURL(t *testing.T) {
	s := &server{}
	m := localMeta{
		LocalHex: "abc123",
		Path:     "/media/Test Episode.mkv",
	}

	got := s.localFileStreamURL(m)
	if !strings.HasPrefix(got, "file://") {
		t.Fatalf("got %q, want file:// URL", got)
	}
}

func TestLocalAddonFileSupportsRangeRequests(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "episode.mkv")
	content := []byte("0123456789abcdef")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}

	oldCache, oldCacheAt := scanCache, scanCacheAt
	t.Cleanup(func() {
		scanCacheMu.Lock()
		scanCache = oldCache
		scanCacheAt = oldCacheAt
		scanCacheMu.Unlock()
	})

	scanCacheMu.Lock()
	scanCache = []localMeta{{
		LocalHex: "abc123",
		Name:     "Episode",
		Path:     path,
		Type:     "series",
	}}
	scanCacheAt = time.Now()
	scanCacheMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/local-addon/file/abc123", nil)
	req.Header.Set("Range", "bytes=2-5")
	rr := httptest.NewRecorder()

	(&server{cfg: types.Config{LocalFilesPublicURL: "http://192.168.1.50:11470"}}).localAddonFile(rr, req, "abc123")

	res := rr.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusPartialContent)
	}
	if got := res.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges = %q, want bytes", got)
	}
	if got := res.Header.Get("Content-Range"); got != "bytes 2-5/16" {
		t.Fatalf("Content-Range = %q, want %q", got, "bytes 2-5/16")
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(body), "2345"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestLocalAddonFileRejectsUnknownHash(t *testing.T) {
	oldCache, oldCacheAt := scanCache, scanCacheAt
	t.Cleanup(func() {
		scanCacheMu.Lock()
		scanCache = oldCache
		scanCacheAt = oldCacheAt
		scanCacheMu.Unlock()
	})

	scanCacheMu.Lock()
	scanCache = []localMeta{{
		LocalHex: "known",
		Path:     "/does/not/matter.mkv",
	}}
	scanCacheAt = time.Now()
	scanCacheMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/local-addon/file/unknown", nil)
	rr := httptest.NewRecorder()

	(&server{cfg: types.Config{LocalFilesPublicURL: "http://192.168.1.50:11470"}}).localAddonFile(rr, req, "unknown")

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNotFound)
	}
}

// TestLocalAddonFileGating pins that the file endpoint is opt-in (404 unless
// STREMIO_LOCAL_FILES_PUBLIC_URL is set) and read-only (GET/HEAD).
func TestLocalAddonFileGating(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}

	oldCache, oldCacheAt := scanCache, scanCacheAt
	t.Cleanup(func() {
		scanCacheMu.Lock()
		scanCache = oldCache
		scanCacheAt = oldCacheAt
		scanCacheMu.Unlock()
	})
	scanCacheMu.Lock()
	scanCache = []localMeta{{LocalHex: "h1", Path: path}}
	scanCacheAt = time.Now()
	scanCacheMu.Unlock()

	on := types.Config{LocalFilesPublicURL: "http://192.168.1.50:11470"}
	cases := []struct {
		name   string
		cfg    types.Config
		method string
		want   int
	}{
		{"disabled GET", types.Config{}, http.MethodGet, http.StatusNotFound},
		{"disabled HEAD", types.Config{}, http.MethodHead, http.StatusNotFound},
		{"enabled GET", on, http.MethodGet, http.StatusOK},
		{"enabled HEAD", on, http.MethodHead, http.StatusOK},
		{"enabled POST", on, http.MethodPost, http.StatusMethodNotAllowed},
		{"enabled DELETE", on, http.MethodDelete, http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/local-addon/file/h1", nil)
			rr := httptest.NewRecorder()
			(&server{cfg: tc.cfg}).localAddonFile(rr, req, "h1")
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d", rr.Code, tc.want)
			}
		})
	}
}
