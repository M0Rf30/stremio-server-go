// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	lzstring "github.com/daku10/go-lz-string"
)

// apiFakeFTP is a minimal loopback FTP server (login, SIZE, EPSV, REST, RETR)
// that counts what the /ftp handler cost it. noSize makes SIZE fail with 502.
type apiFakeFTP struct {
	ln      net.Listener
	content []byte
	noSize  bool

	conns, logins, sizes, rests, retrs atomic.Int64
}

func newAPIFakeFTP(t *testing.T, content []byte, noSize bool) *apiFakeFTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &apiFakeFTP{ln: ln, content: content, noSize: noSize}
	done := make(chan struct{})
	t.Cleanup(func() { close(done); _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c, done)
		}
	}()
	return f
}

func (f *apiFakeFTP) serve(c net.Conn, done <-chan struct{}) {
	defer func() { _ = c.Close() }()
	go func() { <-done; _ = c.Close() }()
	f.conns.Add(1)
	w := func(s string) { _, _ = io.WriteString(c, s+"\r\n") }
	w("220 fake ready")
	var dataLn net.Listener
	closeData := func() {
		if dataLn != nil {
			_ = dataLn.Close()
			dataLn = nil
		}
	}
	defer closeData()
	var restOff int64
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd, arg, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch strings.ToUpper(cmd) {
		case "USER":
			w("331 need password")
		case "PASS":
			f.logins.Add(1)
			w("230 logged in")
		case "TYPE", "NOOP":
			w("200 ok")
		case "SIZE":
			f.sizes.Add(1)
			if f.noSize {
				w("502 SIZE not implemented")
				continue
			}
			w(fmt.Sprintf("213 %d", len(f.content)))
		case "EPSV":
			closeData()
			dataLn, err = net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				w("425 no data")
				continue
			}
			w(fmt.Sprintf("229 Entering Extended Passive Mode (|||%d|)", dataLn.Addr().(*net.TCPAddr).Port))
		case "REST":
			f.rests.Add(1)
			restOff, _ = strconv.ParseInt(arg, 10, 64)
			w("350 ok")
		case "RETR":
			f.retrs.Add(1)
			if dataLn == nil {
				w("425 no data")
				continue
			}
			w("150 opening")
			dc, err := dataLn.Accept()
			closeData()
			if err != nil {
				return
			}
			off := restOff
			if off < 0 || off > int64(len(f.content)) {
				off = int64(len(f.content))
			}
			body := f.content[off:]
			restOff = 0
			_, werr := dc.Write(body)
			_ = dc.Close()
			if werr != nil {
				w("426 transfer aborted")
				continue
			}
			w("226 done")
		case "QUIT":
			w("221 bye")
			return
		default:
			w("502 not implemented")
		}
	}
}

func ftpLZPath(t *testing.T, ftpURL string) string {
	t.Helper()
	compressed, err := lzstring.CompressToEncodedURIComponent(fmt.Sprintf(`{"ftpUrl":"%s"}`, ftpURL))
	if err != nil {
		t.Fatalf("lz compress: %v", err)
	}
	return "/ftp/video.mkv?lz=" + url.QueryEscape(compressed)
}

// TestHandlerFTP_SequentialRangesReuseSession drives N sequential ranged GETs
// through the real handler against an FTP server and asserts they share one
// logged-in control connection (before pooling: N dials, N logins, N SIZE).
func TestHandlerFTP_SequentialRangesReuseSession(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	data := make([]byte, 5000)
	for i := range data {
		data[i] = byte('a' + i%26)
	}
	srv := newAPIFakeFTP(t, data, false)
	path := ftpLZPath(t, "ftp://"+srv.ln.Addr().String()+"/video.mkv")
	h := newHandler(t)

	starts := []int{0, 4000, 100, 3000, 2500, 50, 4500, 10}
	for _, s := range starts {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", s, s+31))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusPartialContent {
			t.Fatalf("start=%d status = %d; want 206 (body=%q)", s, rec.Code, rec.Body.String())
		}
		if want := fmt.Sprintf("bytes %d-%d/%d", s, s+31, len(data)); rec.Header().Get("Content-Range") != want {
			t.Errorf("start=%d Content-Range = %q; want %q", s, rec.Header().Get("Content-Range"), want)
		}
		if !bytes.Equal(rec.Body.Bytes(), data[s:s+32]) {
			t.Errorf("start=%d body = %q; want %q", s, rec.Body.Bytes(), data[s:s+32])
		}
	}
	t.Logf("requests=%d control_conns=%d logins=%d size_cmds=%d retr=%d",
		len(starts), srv.conns.Load(), srv.logins.Load(), srv.sizes.Load(), srv.retrs.Load())
	if srv.conns.Load() != 1 || srv.logins.Load() != 1 {
		t.Errorf("control conns = %d, logins = %d; want 1 and 1 for %d sequential requests",
			srv.conns.Load(), srv.logins.Load(), len(starts))
	}
	if srv.sizes.Load() != 1 {
		t.Errorf("SIZE commands = %d; want 1 (cached for seeks)", srv.sizes.Load())
	}
	if srv.retrs.Load() != int64(len(starts)) {
		t.Errorf("RETR = %d; want %d", srv.retrs.Load(), len(starts))
	}
}

// TestHandlerFTP_UnknownSizeRangeOpensOnce covers the double open: an FTP
// server without SIZE cannot yield a valid Content-Range, so a ranged GET is
// answered 200 with the full body — and must cost a single login + RETR from
// byte 0 (previously: RETR seeked to the offset, closed, then a second login
// and RETR from 0).
func TestHandlerFTP_UnknownSizeRangeOpensOnce(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	data := make([]byte, 1000)
	for i := range data {
		data[i] = byte('a' + i%26)
	}
	srv := newAPIFakeFTP(t, data, true)
	path := ftpLZPath(t, "ftp://"+srv.ln.Addr().String()+"/video.mkv")

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Range", "bytes=100-199")
	rec := httptest.NewRecorder()
	newHandler(t).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (size unknown => no 206)", rec.Code)
	}
	if rec.Header().Get("Content-Range") != "" {
		t.Errorf("Content-Range = %q; want none", rec.Header().Get("Content-Range"))
	}
	if !bytes.Equal(rec.Body.Bytes(), data) {
		t.Errorf("body len = %d; want full %d bytes from byte 0", rec.Body.Len(), len(data))
	}
	t.Logf("control_conns=%d logins=%d retr=%d rest=%d",
		srv.conns.Load(), srv.logins.Load(), srv.retrs.Load(), srv.rests.Load())
	if srv.logins.Load() != 1 || srv.retrs.Load() != 1 || srv.rests.Load() != 0 {
		t.Errorf("logins = %d, RETR = %d, REST = %d; want 1, 1, 0 (single open from byte 0)",
			srv.logins.Load(), srv.retrs.Load(), srv.rests.Load())
	}
}

// TestHandlerFTP_HTTPUnknownSizeStillFallsBack keeps the HTTP behaviour
// identical: a source that reports no total size (chunked, Range ignored) is
// answered 200 with the whole body.
func TestHandlerFTP_HTTPUnknownSizeStillFallsBack(t *testing.T) {
	t.Setenv("STREMIO_FTP_ALLOW_PRIVATE", "1")
	data := []byte(strings.Repeat("0123456789", 100))
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush() // chunked: no Content-Length
		_, _ = w.Write(data)
	}))
	defer src.Close()

	req := httptest.NewRequest(http.MethodGet, ftpLZPath(t, src.URL+"/video.mkv"), nil)
	req.Header.Set("Range", "bytes=10-")
	rec := httptest.NewRecorder()
	newHandler(t).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), data) {
		t.Fatalf("status = %d, body len = %d; want 200 with the full %d bytes", rec.Code, rec.Body.Len(), len(data))
	}
}
