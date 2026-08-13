// Package api — tests for the /ftp handler's Range header framing.
package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	lzstring "github.com/daku10/go-lz-string"
)

// newFakeFTPSource starts a local HTTP server that ftpstream.Open can treat
// as the remote resource. Unlike a strict RFC 7233 server, a start offset at
// or beyond the resource size still succeeds with the true total size and an
// empty body — matching how an FTP server's SIZE/REST/RETR sequence behaves
// for an out-of-range REST offset, which is the scenario handleFTP relies on
// to produce a 416 (rather than a 502 stream-open failure).
func newFakeFTPSource(data []byte) *httptest.Server {
	total := int64(len(data))
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHdr := r.Header.Get("Range")
		if rangeHdr == "" {
			w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
		spec := strings.TrimSuffix(strings.TrimPrefix(rangeHdr, "bytes="), "-")
		start, err := strconv.ParseInt(spec, 10, 64)
		if err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		if start >= total {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, total-1, total))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start:])
	}))
}

// TestHandlerFTP_RangeFraming covers the framing-agreement fix: whatever
// ftpExtractRangeStart decides about the seek offset must match what
// parseRange decides about the response status/Content-Length, so the
// declared Content-Length always equals the actual body length.
func TestHandlerFTP_RangeFraming(t *testing.T) {
	data := make([]byte, 1000)
	for i := range data {
		data[i] = byte('a' + i%26)
	}
	src := newFakeFTPSource(data)
	defer src.Close()

	compressed, err := lzstring.CompressToEncodedURIComponent(fmt.Sprintf(`{"ftpUrl":"%s/video.mkv"}`, src.URL))
	if err != nil {
		t.Fatalf("lz compress: %v", err)
	}
	path := "/ftp/video.mkv?lz=" + url.QueryEscape(compressed)

	tests := []struct {
		name             string
		rangeHdr         string
		wantStatus       int
		wantContentLen   string
		wantContentRange string
	}{
		{
			name:             "malformed end token treated as absent",
			rangeHdr:         "bytes=100-xyz",
			wantStatus:       http.StatusOK,
			wantContentLen:   "1000",
			wantContentRange: "",
		},
		{
			name:             "well-formed explicit range",
			rangeHdr:         "bytes=100-199",
			wantStatus:       http.StatusPartialContent,
			wantContentLen:   "100",
			wantContentRange: "bytes 100-199/1000",
		},
		{
			name:             "open-ended range",
			rangeHdr:         "bytes=100-",
			wantStatus:       http.StatusPartialContent,
			wantContentLen:   "900",
			wantContentRange: "bytes 100-999/1000",
		},
		{
			name:             "unsatisfiable range",
			rangeHdr:         "bytes=99999999-",
			wantStatus:       http.StatusRequestedRangeNotSatisfiable,
			wantContentLen:   "",
			wantContentRange: "bytes */1000",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHandler(t)
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Range", tc.rangeHdr)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d; want %d (body=%q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Length"); got != tc.wantContentLen {
				t.Errorf("Content-Length = %q; want %q", got, tc.wantContentLen)
			}
			if got := rec.Header().Get("Content-Range"); got != tc.wantContentRange {
				t.Errorf("Content-Range = %q; want %q", got, tc.wantContentRange)
			}
			if tc.wantContentLen != "" {
				wantLen, err := strconv.Atoi(tc.wantContentLen)
				if err != nil {
					t.Fatalf("bad wantContentLen fixture %q: %v", tc.wantContentLen, err)
				}
				if rec.Body.Len() != wantLen {
					t.Errorf("body length = %d; want %d (declared Content-Length %d) — framing mismatch", rec.Body.Len(), wantLen, wantLen)
				}
			} else if rec.Body.Len() != 0 {
				t.Errorf("body length = %d; want 0 for %d response", rec.Body.Len(), tc.wantStatus)
			}
		})
	}
}
