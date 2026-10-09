// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package archive_test

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/M0Rf30/stremio-server-go/internal/archive"
)

// compressibleBytes is n bytes that deflate well: words picked pseudo-randomly
// from a small vocabulary.
func compressibleBytes(n int, seed uint64) []byte {
	words := [][]byte{[]byte("stream "), []byte("archive "), []byte("matroska "), []byte("subtitle "), []byte("torrent "), []byte("x")}
	noise := noiseBytes(n/4+1, seed)
	var buf bytes.Buffer
	for i := 0; buf.Len() < n; i++ {
		buf.Write(words[int(noise[i%len(noise)])%len(words)])
	}
	return buf.Bytes()[:n]
}

func benchZip(b *testing.B, method uint16, data []byte) string {
	b.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "m.bin", Method: method})
	if err != nil {
		b.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		b.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		b.Fatal(err)
	}
	p := filepath.Join(b.TempDir(), "bench.zip")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		b.Fatal(err)
	}
	return p
}

// BenchmarkExtract is the throughput of reading one zip entry end to end — the
// extraction path — through a Reader that keeps no file handle between reads:
// it guards the cost of opening the archive once per read-ahead block.
func BenchmarkExtract(b *testing.B) {
	const size = 32 << 20
	for _, tc := range []struct {
		name   string
		method uint16
	}{
		{"deflated", zip.Deflate},
		{"stored", zip.Store},
	} {
		b.Run(tc.name, func(b *testing.B) {
			p := benchZip(b, tc.method, compressibleBytes(size, 5))
			b.SetBytes(size)
			b.ReportAllocs()
			for b.Loop() {
				r, err := archive.OpenFile(p, "zip")
				if err != nil {
					b.Fatal(err)
				}
				rc, err := r.Open("m.bin")
				if err != nil {
					b.Fatal(err)
				}
				if _, err := io.Copy(io.Discard, rc); err != nil {
					b.Fatal(err)
				}
				_ = rc.Close()
				_ = r.Close()
			}
		})
	}
}
