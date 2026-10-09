// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// archiveBenchMB sizes the entry used by the archive TTFB benchmarks:
//
//	go test -run '^$' -bench ArchiveTTFB -benchtime=3x ./internal/api/ -args -archive.bench.mb=512
var archiveBenchMB = flag.Int64("archive.bench.mb", 32, "size in MiB of the archive entry used by BenchmarkArchiveTTFB")

// benchFill writes size bytes of deterministic, moderately compressible data
// (so deflate/gzip have real work to do on the way out, unlike pure noise which
// the encoder would emit as stored blocks).
func benchFill(b *testing.B, w io.Writer, size int64, seed uint64) {
	b.Helper()
	rng := rand.New(rand.NewPCG(seed, 2))
	words := make([][]byte, 4096)
	for i := range words {
		wd := make([]byte, 4+rng.IntN(12))
		for j := range wd {
			wd[j] = byte(rng.IntN(256))
		}
		words[i] = wd
	}
	buf := make([]byte, 0, 1<<20)
	var written int64
	for written < size {
		buf = buf[:0]
		for len(buf) < 1<<20-32 && written+int64(len(buf)) < size {
			buf = append(buf, words[rng.IntN(len(words))]...)
		}
		if rem := size - written; int64(len(buf)) > rem {
			buf = buf[:rem]
		}
		if _, err := w.Write(buf); err != nil {
			b.Fatal(err)
		}
		written += int64(len(buf))
	}
}

// writeBenchZip writes a one-entry zip using the requested zip method and
// returns the archive path.
func writeBenchZip(b *testing.B, dir, entry string, method uint16, size int64) string {
	b.Helper()
	p := filepath.Join(dir, "bench.zip")
	f, err := os.Create(p)
	if err != nil {
		b.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: entry, Method: method})
	if err != nil {
		b.Fatal(err)
	}
	benchFill(b, w, size, 1)
	if err := zw.Close(); err != nil {
		b.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
	return p
}

// writeBenchTgz writes a .tar.gz whose first entry is a size-byte filler and
// whose second entry (the one streamed) is size bytes too, so reaching it costs
// a full decompression of everything before it — the worst case for the
// "rescan from the start" findings.
func writeBenchTgz(b *testing.B, dir, entry string, size int64) string {
	b.Helper()
	p := filepath.Join(dir, "bench.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		b.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	for i, name := range []string{"filler.bin", entry} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: size}); err != nil {
			b.Fatal(err)
		}
		benchFill(b, tw, size, uint64(i+1))
	}
	if err := tw.Close(); err != nil {
		b.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		b.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
	return p
}

// benchArchiveTTFB measures the wall time from sending a ranged GET for a big
// archive entry until the first response body byte arrives. Each iteration
// uses a fresh session so nothing is served from a previous extraction.
func benchArchiveTTFB(b *testing.B, ext string, write func(b *testing.B, dir, entry string, size int64) string, rangeHdr func(size int64) string) {
	size := *archiveBenchMB << 20
	dir := b.TempDir()
	const entry = "big.mkv"
	archivePath := write(b, dir, entry, size)
	b.Setenv("STREMIO_ARCHIVE_LOCAL_ROOT", dir)

	srv := httptest.NewServer(benchHandler())
	defer srv.Close()
	client := &http.Client{}

	var total time.Duration
	b.ResetTimer()
	for i := range b.N {
		b.StopTimer()
		key := fmt.Sprintf("bench-ttfb-%s-%d", b.Name(), i)
		body := fmt.Sprintf(`{"url":%q}`, archivePath)
		resp, err := client.Post(srv.URL+"/"+ext+"/create/"+key, "application/json", strings.NewReader(body))
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b.Fatalf("create status %d", resp.StatusCode)
		}
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/"+ext+"/stream/"+key+"/"+entry, nil)
		if err != nil {
			b.Fatal(err)
		}
		req.Header.Set("Range", rangeHdr(size))
		b.StartTimer()

		start := time.Now()
		resp, err = client.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		var first [1]byte
		if _, err := io.ReadFull(resp.Body, first[:]); err != nil {
			b.Fatalf("first byte: %v", err)
		}
		total += time.Since(start)
		if resp.StatusCode != http.StatusPartialContent {
			b.Fatalf("stream status %d", resp.StatusCode)
		}
		_ = resp.Body.Close()

		b.StopTimer()
		archiveBenchRelease(key)
		b.StartTimer()
	}
	b.ReportMetric(float64(total.Microseconds())/1000/float64(b.N), "ttfb-ms/op")
}

// archiveBenchRelease waits for any background extraction of the session to
// finish, then drops the session and its temp files.
func archiveBenchRelease(key string) {
	archiveSessionsMu.Lock()
	sess := archiveSessions[key]
	delete(archiveSessions, key)
	archiveSessionsMu.Unlock()
	if sess == nil {
		return
	}
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		sess.mu.Lock()
		n := len(sess.inflight)
		sess.mu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	_ = os.RemoveAll(sess.tmpDir)
}

func archiveBenchHead(int64) string { return "bytes=0-1023" }
func archiveBenchMid(size int64) string {
	return "bytes=" + fmt.Sprint(size/2) + "-" + fmt.Sprint(size/2+1023)
}

func benchZipWriter(method uint16) func(*testing.B, string, string, int64) string {
	return func(b *testing.B, dir, entry string, size int64) string {
		return writeBenchZip(b, dir, entry, method, size)
	}
}

func BenchmarkArchiveTTFB_StoredHead(b *testing.B) {
	benchArchiveTTFB(b, "zip", benchZipWriter(zip.Store), archiveBenchHead)
}

func BenchmarkArchiveTTFB_StoredMid(b *testing.B) {
	benchArchiveTTFB(b, "zip", benchZipWriter(zip.Store), archiveBenchMid)
}

func BenchmarkArchiveTTFB_DeflatedHead(b *testing.B) {
	benchArchiveTTFB(b, "zip", benchZipWriter(zip.Deflate), archiveBenchHead)
}

// TgzSecondEntry streams the second of two equally big entries of a .tar.gz:
// the server has to decompress past the first one regardless.
func BenchmarkArchiveTTFB_TgzSecondEntry(b *testing.B) {
	benchArchiveTTFB(b, "tgz", writeBenchTgz, archiveBenchHead)
}

// BenchmarkArchiveThroughput_Stored drains a whole stored entry over loopback
// TCP (no Range) and reports the steady-state rate after the first byte, so a
// serving path that gives up sendfile/zero-copy shows up as a throughput drop.
func BenchmarkArchiveThroughput_Stored(b *testing.B) {
	size := *archiveBenchMB << 20
	dir := b.TempDir()
	const entry = "big.mkv"
	archivePath := writeBenchZip(b, dir, entry, zip.Store, size)
	b.Setenv("STREMIO_ARCHIVE_LOCAL_ROOT", dir)

	srv := httptest.NewServer(benchHandler())
	defer srv.Close()
	client := &http.Client{}

	key := "bench-throughput-" + b.Name()
	resp, err := client.Post(srv.URL+"/zip/create/"+key, "application/json",
		strings.NewReader(fmt.Sprintf(`{"url":%q}`, archivePath)))
	if err != nil {
		b.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	defer archiveBenchRelease(key)

	var drain time.Duration
	b.ResetTimer()
	for range b.N {
		resp, err := client.Get(srv.URL + "/zip/stream/" + key + "/" + entry)
		if err != nil {
			b.Fatal(err)
		}
		var first [1]byte
		if _, err := io.ReadFull(resp.Body, first[:]); err != nil {
			b.Fatal(err)
		}
		start := time.Now()
		n, err := io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if err != nil || n+1 != size {
			b.Fatalf("drain: n=%d err=%v", n+1, err)
		}
		drain += time.Since(start)
	}
	b.ReportMetric(float64(size)*float64(b.N)/drain.Seconds()/(1<<20), "MiB/s")
}
