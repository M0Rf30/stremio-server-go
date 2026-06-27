// Package api — FTP/FTPS and HTTP(S) direct streaming via the /ftp route.
//
// GET|HEAD /ftp/{filename}?lz=<encoded>
//
// The lz parameter is an lz-string (compressToEncodedURIComponent) encoded
// JSON object {"ftpUrl":"<url>"} where url may be ftp://, ftps://, http://, or
// https://. The file is streamed to the client with Accept-Ranges / 206 partial
// content support when the total resource size is known.
//
// Session routes respond 501 — the direct ?lz route is the supported path:
//
//	/ftp/create, /ftp/create/{key}
//	/ftp/stream, /ftp/stream/{key}/{file...}
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	lzstring "github.com/daku10/go-lz-string"

	"github.com/M0Rf30/stremio-server-go/internal/ftpstream"
)

// ftpPayload is the JSON structure carried in the lz-encoded query parameter.
type ftpPayload struct {
	FtpURL string `json:"ftpUrl"`
}

// ftpDLNAHeaders sets the DLNA streaming headers required by Stremio clients.
func ftpDLNAHeaders(h http.Header) {
	h.Set("transferMode.dlna.org", "Streaming")
	h.Set("contentFeatures.dlna.org", "DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000")
}

// handleFTP dispatches /ftp/* requests.
//
// Supported route:
//
//	GET|HEAD /ftp/{filename}?lz=<encoded>  — stream the URL encoded in lz
//
// Intentionally unsupported routes (501):
//
//	GET|POST /ftp/create
//	GET|POST /ftp/create/{key}
//	GET|POST /ftp/stream
//	GET|POST /ftp/stream/{key}/{file...}
//
// @Summary  Stream a remote FTP, FTPS, or HTTP(S) file
// @Tags     Streaming
// @Param    filename  path   string  true   "media filename for MIME-type detection"
// @Param    lz        query  string  true   "lz-string encoded {\"ftpUrl\":\"...\"}"
// @Param    Range     header string  false  "byte range (RFC 7233)"
// @Success  200  {string}  string  "full streaming content"
// @Success  206  {string}  string  "partial content"
// @Failure  400
// @Failure  501
// @Failure  502
// @Router   /ftp/{filename} [get]
func (s *server) handleFTP(w http.ResponseWriter, r *http.Request, seg []string) {
	// seg[0] == "ftp"; seg[1] is the next path component (if present).
	if len(seg) >= 2 && (seg[1] == "create" || seg[1] == "stream") {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error": "ftp session routes are not supported; use GET /ftp/{filename}?lz=<encoded>",
		})
		return
	}

	if len(seg) < 2 || seg[1] == "" {
		http.NotFound(w, r)
		return
	}

	filename := seg[1]

	lzParam := r.URL.Query().Get("lz")
	if lzParam == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "lz query parameter is required",
		})
		return
	}

	jsonStr, err := lzstring.DecompressFromEncodedURIComponent(lzParam)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "lz decode error: " + err.Error(),
		})
		return
	}

	var payload ftpPayload
	if err := json.Unmarshal([]byte(jsonStr), &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid JSON in lz payload: " + err.Error(),
		})
		return
	}
	if payload.FtpURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "ftpUrl is required in the lz payload",
		})
		return
	}

	// Seek to start for the common case (FTP REST / HTTP Range). We don't know
	// the total size until after Open returns, so we open optimistically and
	// reopen at 0 if size is unknown — a valid 206 Content-Range requires the
	// total size, so we must fall back to 200 when it is unavailable.
	rangeHdr := r.Header.Get("Range")
	start, hasRange := ftpExtractRangeStart(rangeHdr)

	rc, size, err := ftpstream.Open(r.Context(), payload.FtpURL, start)
	if err != nil {
		http.Error(w, "stream open: "+err.Error(), http.StatusBadGateway)
		return
	}
	// Range was requested and we seeked ahead, but total size is unknown so a
	// valid Content-Range header cannot be emitted. Close this connection and
	// reopen from byte 0; serve 200 instead.
	if hasRange && size < 0 && start > 0 {
		_ = rc.Close()
		rc, size, err = ftpstream.Open(r.Context(), payload.FtpURL, 0)
		if err != nil {
			http.Error(w, "stream open: "+err.Error(), http.StatusBadGateway)
			return
		}
		hasRange = false
	}
	defer func() { _ = rc.Close() }()

	hdr := w.Header()
	hdr.Set("Content-Type", mimeByName(filename))
	hdr.Set("Accept-Ranges", "bytes")
	ftpDLNAHeaders(hdr)

	// Serve 206 Partial Content when the range is satisfiable and the total
	// resource size is known.
	if hasRange && size >= 0 {
		rStart, rEnd, ok, unsat := parseRange(rangeHdr, size)
		if unsat {
			hdr.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if ok {
			copyLen := rEnd - rStart + 1
			hdr.Set("Content-Length", strconv.FormatInt(copyLen, 10))
			hdr.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rStart, rEnd, size))
			w.WriteHeader(http.StatusPartialContent)
			if r.Method == http.MethodHead {
				return
			}
			bufp := streamBufPool.Get().(*[]byte)
			defer streamBufPool.Put(bufp)
			_, _ = io.CopyBuffer(w, io.LimitReader(rc, copyLen), *bufp)
			return
		}
	}

	// 200 full or unbounded streaming response.
	if size >= 0 {
		hdr.Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	bufp := streamBufPool.Get().(*[]byte)
	defer streamBufPool.Put(bufp)
	_, _ = io.CopyBuffer(w, rc, *bufp)
}

// ftpExtractRangeStart parses the start byte offset from a Range header for
// use as the initial read position before the total resource size is known.
//
// Only explicit start ranges ("bytes=N-" or "bytes=N-M") are supported.
// Suffix ranges ("bytes=-N") return ok=false because the absolute start byte
// cannot be determined without the total size.
func ftpExtractRangeStart(h string) (start int64, ok bool) {
	if !strings.HasPrefix(h, "bytes=") {
		return 0, false
	}
	spec := strings.TrimPrefix(h, "bytes=")
	// Multi-range: use only the first range.
	if i := strings.IndexByte(spec, ','); i >= 0 {
		spec = spec[:i]
	}
	dash := strings.IndexByte(spec, '-')
	// dash <= 0: no dash found, or suffix range like "bytes=-N" (dash at index 0).
	if dash <= 0 {
		return 0, false
	}
	s, err := strconv.ParseInt(spec[:dash], 10, 64)
	if err != nil || s < 0 {
		return 0, false
	}
	return s, true
}
