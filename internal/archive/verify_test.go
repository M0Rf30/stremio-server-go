// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package archive_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/M0Rf30/stremio-server-go/internal/archive"
)

func openRaw(t *testing.T, p string) *os.File {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestZipLocate_ReportsCRCAndVerifies(t *testing.T) {
	// Sizes straddle the internal read buffer (256 KiB) in both directions.
	sizes := []int{1, 4096, 256 << 10, 256<<10 + 1, 600_000}
	var specs []zipSpec
	datas := map[string][]byte{}
	for i, n := range sizes {
		name := string(rune('a'+i)) + ".bin"
		datas[name] = noiseBytes(n, uint64(40+i))
		specs = append(specs, zipSpec{name: name, method: zip.Store, data: datas[name]})
	}
	p := writeZipSpecs(t, specs)
	_, loc := mustLocator(t, p, "zip")
	f := openRaw(t, p)

	for name, data := range datas {
		ext, ok := loc.Locate(name)
		if !ok {
			t.Fatalf("Locate(%q) = false", name)
		}
		if !ext.HasCRC || ext.CRC32 != crc32.ChecksumIEEE(data) {
			t.Errorf("Locate(%q): HasCRC=%v CRC32=%08x, want true/%08x", name, ext.HasCRC, ext.CRC32, crc32.ChecksumIEEE(data))
		}
		if err := ext.Verify(context.Background(), f); err != nil {
			t.Errorf("Verify(%q) on a healthy entry: %v", name, err)
		}
	}
}

func TestExtentVerify_DetectsCorruptionLikeZipOpen(t *testing.T) {
	data := noiseBytes(700_000, 50)
	p := writeZipSpecs(t, []zipSpec{{name: "a.bin", method: zip.Store, data: data}})
	_, loc := mustLocator(t, p, "zip")
	ext, ok := loc.Locate("a.bin")
	if !ok {
		t.Fatal("Locate = false")
	}
	for _, at := range []int64{0, 300_000, ext.Size - 1} { // first, middle, last byte
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		raw[ext.Offset+at] ^= 0x01
		bad := filepath.Join(t.TempDir(), "bad.zip")
		if err := os.WriteFile(bad, raw, 0o600); err != nil {
			t.Fatal(err)
		}

		err = ext.Verify(context.Background(), openRaw(t, bad))
		if !errors.Is(err, archive.ErrChecksum) || !errors.Is(err, zip.ErrChecksum) {
			t.Errorf("flip at %d: Verify = %v, want ErrChecksum", at, err)
		}

		// zip's own reader reports the same failure for the same bytes.
		r, err := archive.OpenFile(bad, "zip")
		if err != nil {
			t.Fatal(err)
		}
		rc, err := r.Open("a.bin")
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(io.Discard, rc)
		_ = rc.Close()
		_ = r.Close()
		if !errors.Is(err, zip.ErrChecksum) {
			t.Errorf("flip at %d: Open+read = %v, want zip.ErrChecksum", at, err)
		}
	}
}

func TestExtentVerify_ShortFileAndCancel(t *testing.T) {
	data := noiseBytes(500_000, 51)
	p := writeZipSpecs(t, []zipSpec{{name: "a.bin", method: zip.Store, data: data}})
	_, loc := mustLocator(t, p, "zip")
	ext, _ := loc.Locate("a.bin")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}

	// The archive file is shorter than the extent claims.
	short := bytes.NewReader(raw[:ext.Offset+ext.Size-10])
	if err := ext.Verify(context.Background(), short); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("short file: Verify = %v, want io.ErrUnexpectedEOF", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ext.Verify(ctx, bytes.NewReader(raw)); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: Verify = %v, want context.Canceled", err)
	}
}

// An archive that records no checksum has nothing to verify (zip.File.Open
// skips the comparison then too), and tar never records one.
func TestExtentVerify_NoRecordedChecksumIsNotChecked(t *testing.T) {
	data := noiseBytes(2000, 52)
	p := filepath.Join(t.TempDir(), "nocrc.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	// Raw header: stored, no data descriptor, CRC-32 left at 0 (unset).
	w, err := zw.CreateRaw(&zip.FileHeader{
		Name: "a.bin", Method: zip.Store, CompressedSize64: uint64(len(data)), UncompressedSize64: uint64(len(data)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	r, loc := mustLocator(t, p, "zip")
	ext, ok := loc.Locate("a.bin")
	if !ok || ext.HasCRC {
		t.Fatalf("Locate = %+v, %v; want a located extent without a recorded CRC", ext, ok)
	}
	if err := ext.Verify(context.Background(), openRaw(t, p)); err != nil {
		t.Errorf("Verify with no recorded CRC = %v, want nil", err)
	}
	rc, err := r.Open("a.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, rc); err != nil {
		t.Errorf("zip reader itself rejects it (%v): HasCRC must mirror that", err)
	}
	_ = rc.Close()

	hdrs, bodies := tarFixture()
	tp := plainTar(t, hdrs, bodies, false)
	_, tloc := mustLocator(t, tp, "tar")
	text, ok := tloc.Locate("a.mkv")
	if !ok || text.HasCRC {
		t.Fatalf("tar Locate = %+v, %v; want an extent without CRC", text, ok)
	}
	if err := text.Verify(context.Background(), bytes.NewReader(nil)); err != nil {
		t.Errorf("tar Verify = %v, want nil (nothing recorded)", err)
	}
}
