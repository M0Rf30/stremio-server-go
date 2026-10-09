// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	lzstring "github.com/daku10/go-lz-string"
)

// BenchmarkArchiveCreateSeek models a player (ExoPlayer) that re-requests the
// GET /zip/create/{key}?lz=… stream URL on every open/seek and then ranges into
// the entry: one op = create (307) + a 64 KiB range GET at a moving offset,
// against a remote deflated archive. Before session reuse every op downloaded
// the whole archive again and re-extracted the entry (downloads/op ≈ 1,
// extractions/op ≈ 1); now only the first op does (both ≈ 1/b.N).
func BenchmarkArchiveCreateSeek(b *testing.B) {
	const size = 8 << 20
	b.Setenv("STREMIO_ARCHIVE_ALLOW_PRIVATE", "1")
	tmp := b.TempDir()
	b.Setenv("TMPDIR", tmp)
	b.Setenv("TMP", tmp)
	b.Setenv("TEMP", tmp)

	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i*131 + i>>9)
	}
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "movie.mkv", Method: zip.Deflate})
	if err != nil {
		b.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		b.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		b.Fatal(err)
	}

	var downloads, extractions atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downloads.Add(1)
		_, _ = w.Write(zbuf.Bytes())
	}))
	defer origin.Close()
	archiveExtractTestHook = func(string) { extractions.Add(1) }
	defer func() { archiveExtractTestHook = nil }()
	srv := httptest.NewServer(benchHandler())
	defer srv.Close()

	key := "bench-createseek-" + b.Name()
	lz, err := lzstring.CompressToEncodedURIComponent(fmt.Sprintf(`{"url":%q}`, origin.URL+"/a.zip"))
	if err != nil {
		b.Fatal(err)
	}
	createURL := srv.URL + "/zip/create/" + url.PathEscape(key) + "?lz=" + url.QueryEscape(lz)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	client := &http.Client{Timeout: time.Minute}
	defer archiveBenchRelease(key)

	i := 0
	for b.Loop() {
		resp, err := noFollow.Get(createURL)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusTemporaryRedirect {
			b.Fatalf("create status %d", resp.StatusCode)
		}
		off := int64(i%64) * (size / 64)
		i++
		req, err := http.NewRequest(http.MethodGet, srv.URL+resp.Header.Get("Location"), nil)
		if err != nil {
			b.Fatal(err)
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+64<<10-1))
		rresp, err := client.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		n, err := io.Copy(io.Discard, rresp.Body)
		_ = rresp.Body.Close()
		if err != nil || rresp.StatusCode != http.StatusPartialContent || n != 64<<10 {
			b.Fatalf("range GET: status %d, %d bytes, err %v", rresp.StatusCode, n, err)
		}
	}
	b.ReportMetric(float64(downloads.Load())/float64(b.N), "downloads/op")
	b.ReportMetric(float64(extractions.Load())/float64(b.N), "extractions/op")
}
