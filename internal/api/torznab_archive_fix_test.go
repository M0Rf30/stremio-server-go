// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestTnFetchErrorOmitsAPIKey verifies transport errors never embed the
// apikey query value (url.Error carries the full URL).
func TestTnFetchErrorOmitsAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close() // connection refused
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	_, err := tnFetch(r, addr+"/api?apikey=SECRETKEY123&t=search")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "SECRETKEY123") {
		t.Errorf("error leaks apikey: %v", err)
	}
}

// TestArchiveDownloadSlowerThanShortTimeout verifies large downloads are not
// killed by the short overall timeout used for small fetches.
func TestArchiveDownloadSlowerThanShortTimeout(t *testing.T) {
	t.Setenv("STREMIO_ARCHIVE_ALLOW_PRIVATE", "1")
	old := archiveFetchClient.Timeout
	archiveFetchClient.Timeout = 50 * time.Millisecond
	t.Cleanup(func() { archiveFetchClient.Timeout = old })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("part1"))
		w.(http.Flusher).Flush()
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte("part2"))
	}))
	defer srv.Close()

	name, err := archiveDownload(srv.URL)
	if err != nil {
		t.Fatalf("archiveDownload: %v", err)
	}
	defer func() { _ = os.Remove(name) }()
	b, _ := os.ReadFile(name)
	if string(b) != "part1part2" {
		t.Errorf("body = %q", b)
	}
}
