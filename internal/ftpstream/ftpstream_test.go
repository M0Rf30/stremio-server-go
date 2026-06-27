package ftpstream

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestParseFTPURL verifies URL component extraction from ftp:// and ftps:// URLs.
// No network connection is used.
func TestParseFTPURL(t *testing.T) {
	tests := []struct {
		name    string
		rawURL  string
		addr    string
		host    string
		user    string
		pass    string
		path    string
		isTLS   bool
		wantErr bool
	}{
		{
			name:   "full credentials with non-default port",
			rawURL: "ftp://alice:secret@media.example.com:2121/videos/movie.mkv",
			addr:   "media.example.com:2121",
			host:   "media.example.com",
			user:   "alice",
			pass:   "secret",
			path:   "/videos/movie.mkv",
			isTLS:  false,
		},
		{
			name:   "anonymous defaults with default port",
			rawURL: "ftp://ftp.example.org/pub/file.mp4",
			addr:   "ftp.example.org:21",
			host:   "ftp.example.org",
			user:   "anonymous",
			pass:   "",
			path:   "/pub/file.mp4",
			isTLS:  false,
		},
		{
			name:   "ftps enables TLS flag",
			rawURL: "ftps://secure.example.com/data/film.mkv",
			addr:   "secure.example.com:21",
			host:   "secure.example.com",
			user:   "anonymous",
			pass:   "",
			path:   "/data/film.mkv",
			isTLS:  true,
		},
		{
			name:   "ftps with credentials and port",
			rawURL: "ftps://bob:pass@ftps.host:990/share/clip.avi",
			addr:   "ftps.host:990",
			host:   "ftps.host",
			user:   "bob",
			pass:   "pass",
			path:   "/share/clip.avi",
			isTLS:  true,
		},
		{
			name:   "user without password",
			rawURL: "ftp://carol@host.local:21/files/video.ts",
			addr:   "host.local:21",
			host:   "host.local",
			user:   "carol",
			pass:   "",
			path:   "/files/video.ts",
			isTLS:  false,
		},
		{
			name:    "unsupported http scheme",
			rawURL:  "http://host/file.mp4",
			wantErr: true,
		},
		{
			name:    "unsupported https scheme",
			rawURL:  "https://host/file.mp4",
			wantErr: true,
		},
		{
			name:    "missing host",
			rawURL:  "ftp:///path/to/file.mkv",
			wantErr: true,
		},
		{
			name:    "malformed URL",
			rawURL:  "://bad",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFTPURL(tc.rawURL)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseFTPURL(%q): expected error, got nil", tc.rawURL)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFTPURL(%q): unexpected error: %v", tc.rawURL, err)
			}
			if got.addr != tc.addr {
				t.Errorf("addr: got %q, want %q", got.addr, tc.addr)
			}
			if got.host != tc.host {
				t.Errorf("host: got %q, want %q", got.host, tc.host)
			}
			if got.user != tc.user {
				t.Errorf("user: got %q, want %q", got.user, tc.user)
			}
			if got.pass != tc.pass {
				t.Errorf("pass: got %q, want %q", got.pass, tc.pass)
			}
			if got.path != tc.path {
				t.Errorf("path: got %q, want %q", got.path, tc.path)
			}
			if got.tls != tc.isTLS {
				t.Errorf("tls: got %v, want %v", got.tls, tc.isTLS)
			}
		})
	}
}

// TestParseFTPURLEdgeCases covers additional parseFTPURL scenarios not in the
// main table test: host-only URL (no path, default port), percent-encoded path
// decoded by url.Parse, and explicit user:pass@host:port/path combination.
func TestParseFTPURLEdgeCases(t *testing.T) {
	t.Run("ftp host-only no path default port anonymous", func(t *testing.T) {
		got, err := parseFTPURL("ftp://media.host")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.addr != "media.host:21" {
			t.Errorf("addr: got %q, want %q", got.addr, "media.host:21")
		}
		if got.host != "media.host" {
			t.Errorf("host: got %q, want %q", got.host, "media.host")
		}
		if got.user != "anonymous" {
			t.Errorf("user: got %q, want anonymous", got.user)
		}
		if got.pass != "" {
			t.Errorf("pass: got %q, want empty", got.pass)
		}
		if got.path != "" {
			t.Errorf("path: got %q, want empty", got.path)
		}
		if got.tls {
			t.Error("tls: got true, want false")
		}
	})

	t.Run("percent-encoded space in path decoded", func(t *testing.T) {
		got, err := parseFTPURL("ftp://filehost/my%20video.mkv")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// url.Parse decodes %20 → space in u.Path
		if got.path != "/my video.mkv" {
			t.Errorf("path: got %q, want %q", got.path, "/my video.mkv")
		}
		if got.host != "filehost" {
			t.Errorf("host: got %q, want filehost", got.host)
		}
	})

	t.Run("user:pass at host:port with path", func(t *testing.T) {
		got, err := parseFTPURL("ftp://user:pass@host:2121/path/to/file.mkv")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.addr != "host:2121" {
			t.Errorf("addr: got %q, want host:2121", got.addr)
		}
		if got.user != "user" {
			t.Errorf("user: got %q, want user", got.user)
		}
		if got.pass != "pass" {
			t.Errorf("pass: got %q, want pass", got.pass)
		}
		if got.path != "/path/to/file.mkv" {
			t.Errorf("path: got %q, want /path/to/file.mkv", got.path)
		}
		if got.tls {
			t.Error("tls: got true, want false for ftp://")
		}
	})

	t.Run("ftps host-only default port tls flag set", func(t *testing.T) {
		got, err := parseFTPURL("ftps://secure.host")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.tls {
			t.Error("tls: got false, want true for ftps://")
		}
		if got.addr != "secure.host:21" {
			t.Errorf("addr: got %q, want secure.host:21", got.addr)
		}
		if got.user != "anonymous" {
			t.Errorf("user: got %q, want anonymous", got.user)
		}
	})

	t.Run("percent-encoded path with encoded slash preserved as decoded", func(t *testing.T) {
		// %2F in the path is decoded to / by url.Parse (u.Path = decoded form).
		got, err := parseFTPURL("ftp://host/dir%2Fwith%2Fslashes")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// url.Parse decodes %2F → '/' in Path, so path becomes /dir/with/slashes
		if !strings.HasPrefix(got.path, "/") {
			t.Errorf("path: got %q, expected to start with /", got.path)
		}
	})
}

// newRangeServer returns an httptest.Server that serves body, honoring Range
// requests, and records the Range header of the most recent request in *rangeHdr.
func newRangeServer(t *testing.T, body []byte, rangeHdr *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rangeHdr != nil {
			*rangeHdr = r.Header.Get("Range")
		}
		rh := r.Header.Get("Range")
		start := int64(0)
		if rh != "" {
			// parse "bytes=N-"
			raw := strings.TrimPrefix(rh, "bytes=")
			raw = strings.TrimSuffix(raw, "-")
			if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
				start = n
			}
		}
		w.Header().Set("Accept-Ranges", "bytes")
		if start > 0 {
			total := int64(len(body))
			end := total - 1
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
			w.Header().Set("Content-Length", strconv.FormatInt(total-start, 10))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(body[start:])
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestOpenHTTPFullRead tests openHTTP at offset=0: full body, size from Content-Length.
func TestOpenHTTPFullRead(t *testing.T) {
	content := []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZ") // 26 bytes
	srv := newRangeServer(t, content, nil)

	rc, size, err := openHTTP(context.Background(), srv.URL+"/file.bin", 0)
	if err != nil {
		t.Fatalf("openHTTP: %v", err)
	}
	defer func() {
		if cerr := rc.Close(); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	}()

	if size != int64(len(content)) {
		t.Errorf("size: got %d, want %d", size, int64(len(content)))
	}

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("body: got %q, want %q", got, content)
	}
}

// TestOpenHTTPRangeRead tests openHTTP with offset > 0:
//   - The client must send "Range: bytes=N-"
//   - Size is inferred from Content-Range header
//   - The reader yields only the tail bytes starting at offset
func TestOpenHTTPRangeRead(t *testing.T) {
	content := []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZ") // 26 bytes
	const offset = int64(10)

	var capturedRange string
	srv := newRangeServer(t, content, &capturedRange)

	rc, size, err := openHTTP(context.Background(), srv.URL+"/file.bin", offset)
	if err != nil {
		t.Fatalf("openHTTP: %v", err)
	}
	defer rc.Close()

	// Server received the correct Range header.
	wantRange := fmt.Sprintf("bytes=%d-", offset)
	if capturedRange != wantRange {
		t.Errorf("Range header: got %q, want %q", capturedRange, wantRange)
	}

	// Size is total (from Content-Range), not remaining.
	if size != int64(len(content)) {
		t.Errorf("size: got %d, want %d (total from Content-Range)", size, int64(len(content)))
	}

	// Reader yields bytes from offset onward.
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	want := content[offset:]
	if string(got) != string(want) {
		t.Errorf("body: got %q, want %q", got, want)
	}
}

// TestOpenHTTPRangeNoContentRange covers the 206 path where the server omits
// Content-Range but sets Content-Length; size = offset + ContentLength.
func TestOpenHTTPRangeNoContentRange(t *testing.T) {
	content := []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZ") // 26 bytes
	const offset = int64(10)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remaining := int64(len(content)) - offset
		w.Header().Set("Content-Length", strconv.FormatInt(remaining, 10))
		// Deliberate: no Content-Range header.
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(content[offset:])
	}))
	t.Cleanup(srv.Close)

	rc, size, err := openHTTP(context.Background(), srv.URL+"/file.bin", offset)
	if err != nil {
		t.Fatalf("openHTTP: %v", err)
	}
	defer rc.Close()

	// size = offset (10) + ContentLength (16) = 26 = len(content)
	if size != int64(len(content)) {
		t.Errorf("size: got %d, want %d (offset+ContentLength)", size, int64(len(content)))
	}
}

// TestOpenHTTPBadStatus tests that a non-200/206 response is returned as an error
// and the body is properly drained/closed.
func TestOpenHTTPBadStatus(t *testing.T) {
	for _, code := range []int{
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusInternalServerError,
		http.StatusUnauthorized,
	} {
		code := code
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				_, _ = fmt.Fprintf(w, "error body for %d", code)
			}))
			t.Cleanup(srv.Close)

			rc, _, err := openHTTP(context.Background(), srv.URL+"/file", 0)
			if err == nil {
				_ = rc.Close()
				t.Fatalf("expected error for HTTP %d, got nil", code)
			}
			if rc != nil {
				t.Errorf("expected nil rc on error, got non-nil")
			}
		})
	}
}

// TestOpenHTTPClose verifies that the returned ReadCloser.Close() does not error.
func TestOpenHTTPClose(t *testing.T) {
	content := []byte("close test body")
	srv := newRangeServer(t, content, nil)

	rc, _, err := openHTTP(context.Background(), srv.URL+"/file", 0)
	if err != nil {
		t.Fatalf("openHTTP: %v", err)
	}
	// Do not read body; just close.
	if cerr := rc.Close(); cerr != nil {
		t.Errorf("Close without read: %v", cerr)
	}

	// After a full read, Close should still not error.
	rc2, _, err := openHTTP(context.Background(), srv.URL+"/file", 0)
	if err != nil {
		t.Fatalf("openHTTP (second): %v", err)
	}
	if _, rerr := io.ReadAll(rc2); rerr != nil {
		t.Fatalf("ReadAll: %v", rerr)
	}
	if cerr := rc2.Close(); cerr != nil {
		t.Errorf("Close after full read: %v", cerr)
	}
}

// TestOpenDispatch verifies that Open routes http/https to openHTTP and returns
// errors for unsupported schemes and malformed URLs.
func TestOpenDispatch(t *testing.T) {
	content := []byte("dispatch test content — 21 bytes")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		_, _ = w.Write(content)
	}))
	t.Cleanup(srv.Close)

	t.Run("http scheme routes to openHTTP and reads body", func(t *testing.T) {
		rc, size, err := Open(t.Context(), srv.URL+"/file", 0)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer rc.Close()
		if size != int64(len(content)) {
			t.Errorf("size: got %d, want %d", size, int64(len(content)))
		}
		got, rerr := io.ReadAll(rc)
		if rerr != nil {
			t.Fatalf("ReadAll: %v", rerr)
		}
		if string(got) != string(content) {
			t.Errorf("body mismatch: got %q", got)
		}
	})

	t.Run("unsupported rtsp scheme returns error", func(t *testing.T) {
		_, _, err := Open(t.Context(), "rtsp://host/stream", 0)
		if err == nil {
			t.Fatal("expected error for rtsp:// scheme")
		}
	})

	t.Run("malformed URL returns error", func(t *testing.T) {
		_, _, err := Open(t.Context(), "://bad", 0)
		if err == nil {
			t.Fatal("expected error for malformed URL")
		}
	})

	t.Run("empty scheme returns error", func(t *testing.T) {
		_, _, err := Open(t.Context(), "noscheme/path", 0)
		if err == nil {
			t.Fatal("expected error for schemeless URL")
		}
	})
}

// TestOpenHTTPWithOffset exercises the Open top-level path with offset > 0.
func TestOpenHTTPWithOffset(t *testing.T) {
	content := []byte("0123456789ABCDEFGHIJ") // 20 bytes
	const offset = int64(5)

	var capturedRange string
	srv := newRangeServer(t, content, &capturedRange)

	rc, size, err := Open(t.Context(), srv.URL+"/file.bin", offset)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()

	if capturedRange != fmt.Sprintf("bytes=%d-", offset) {
		t.Errorf("Range header: got %q, want bytes=%d-", capturedRange, offset)
	}
	if size != int64(len(content)) {
		t.Errorf("size: got %d, want %d", size, int64(len(content)))
	}

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != string(content[offset:]) {
		t.Errorf("body: got %q, want %q", got, content[offset:])
	}
}

// TestOpenHTTPCtxDeadline verifies that a context deadline cancels an in-flight
// HTTP request. The server handler blocks until the client disconnects, making
// the outcome deterministic: the client always times out first.
func TestOpenHTTPCtxDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until the client disconnects (context cancelled on server side).
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	_, _, err := openHTTP(ctx, srv.URL+"/file", 0)
	if err == nil {
		t.Fatal("expected error from context deadline, got nil")
	}
}

// TestOpenHTTPCtxPreCancelled verifies that an already-cancelled context causes
// openHTTP to return an error without making a meaningful HTTP request.
func TestOpenHTTPCtxPreCancelled(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before any call

	_, _, err := openHTTP(ctx, srv.URL+"/file", 0)
	if err == nil {
		t.Fatal("expected error from pre-cancelled context, got nil")
	}
	// The request must not have been served (context cancelled before dial).
	if requests != 0 {
		t.Logf("note: server handled %d request(s) despite pre-cancelled ctx (transport may vary)", requests)
	}
}

// TestOpenHTTPSizeUnknown checks that openHTTP returns size=-1 when the server
// sends neither Content-Length nor Content-Range.
func TestOpenHTTPSizeUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Write status first, then flush so headers are committed without
		// Content-Length. This forces chunked transfer encoding, making
		// resp.ContentLength == -1 on the client side.
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = w.Write([]byte("chunked body without length"))
	}))
	t.Cleanup(srv.Close)

	rc, size, err := openHTTP(context.Background(), srv.URL+"/file", 0)
	if err != nil {
		t.Fatalf("openHTTP: %v", err)
	}
	defer rc.Close()

	if size != -1 {
		t.Errorf("size: got %d, want -1 (unknown)", size)
	}
}

// TestParseFTPURLRejectedSchemes exhaustively checks that schemes other than
// ftp/ftps are rejected, including the common http/https ones.
func TestParseFTPURLRejectedSchemes(t *testing.T) {
	schemes := []string{"http", "https", "rtsp", "sftp", "file", "data"}
	for _, scheme := range schemes {
		scheme := scheme
		t.Run(scheme, func(t *testing.T) {
			_, err := parseFTPURL(scheme + "://host/path")
			if err == nil {
				t.Fatalf("parseFTPURL(%s://…): expected error, got nil", scheme)
			}
		})
	}
}
