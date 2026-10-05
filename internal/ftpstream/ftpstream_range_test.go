// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package ftpstream

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestOpenHTTPRangeIgnored verifies that when the upstream ignores Range and
// replies 200 with the full body, the returned reader is still positioned at
// the requested offset.
func TestOpenHTTPRangeIgnored(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	content := []byte("0123456789ABCDEFGHIJ")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	rc, size, err := openHTTP(t.Context(), srv.URL, 5)
	if err != nil {
		t.Fatalf("openHTTP: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if string(got) != string(content[5:]) {
		t.Errorf("body = %q, want %q", got, content[5:])
	}
	if size != int64(len(content)) {
		t.Errorf("size = %d, want %d", size, len(content))
	}
}

// TestOpenHTTPRangeIgnoredPastEnd verifies an offset beyond a Range-ignoring
// upstream's body maps to ErrRangeNotSatisfiable.
func TestOpenHTTPRangeIgnoredPastEnd(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush() // chunked: no Content-Length
		_, _ = w.Write([]byte("short"))
	}))
	defer srv.Close()
	_, _, err := openHTTP(t.Context(), srv.URL, 100)
	if !errors.Is(err, ErrRangeNotSatisfiable) {
		t.Fatalf("err = %v, want ErrRangeNotSatisfiable", err)
	}
}

// TestOpenHTTP416 verifies an upstream 416 maps to ErrRangeNotSatisfiable.
func TestOpenHTTP416(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	}))
	defer srv.Close()
	_, _, err := openHTTP(t.Context(), srv.URL, 10)
	if !errors.Is(err, ErrRangeNotSatisfiable) {
		t.Fatalf("err = %v, want ErrRangeNotSatisfiable", err)
	}
}
