// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package archive_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/M0Rf30/stremio-server-go/internal/archive"
)

// noiseBytes returns n deterministic pseudo-random (incompressible) bytes.
func noiseBytes(n int, seed uint64) []byte {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.UintN(256))
	}
	return b
}

type zipSpec struct {
	name   string
	method uint16
	flags  uint16
	data   []byte
}

func writeZipSpecs(t *testing.T, specs []zipSpec) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "locate.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, s := range specs {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: s.name, Method: s.method, Flags: s.flags})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(s.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustLocator(t *testing.T, fpath, ext string) (archive.Reader, archive.Locator) {
	t.Helper()
	r, err := archive.OpenFile(fpath, ext)
	if err != nil {
		t.Fatalf("OpenFile(%s): %v", ext, err)
	}
	t.Cleanup(func() { _ = r.Close() })
	loc, ok := r.(archive.Locator)
	if !ok {
		t.Fatalf("%s reader does not implement archive.Locator", ext)
	}
	return r, loc
}

func TestZipLocate(t *testing.T) {
	stored := noiseBytes(300_000, 1)
	other := noiseBytes(1234, 2)
	deflated := bytes.Repeat([]byte("compressible "), 5000)
	p := writeZipSpecs(t, []zipSpec{
		{name: "dir/", method: zip.Store},
		{name: "dir/stored.mkv", method: zip.Store, data: stored},
		{name: "other.bin", method: zip.Store, data: other},
		{name: "deflated.txt", method: zip.Deflate, data: deflated},
		{name: "empty.txt", method: zip.Store},
		{name: "crypt.bin", method: zip.Store, flags: 0x1, data: other},
		// Duplicate name: the first (deflated) entry wins, as it does for Open.
		{name: "dup.bin", method: zip.Deflate, data: deflated},
		{name: "dup.bin", method: zip.Store, data: other},
	})
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	_, loc := mustLocator(t, p, "zip")

	for name, want := range map[string][]byte{"dir/stored.mkv": stored, "other.bin": other, "empty.txt": nil} {
		ext, ok := loc.Locate(name)
		if !ok {
			t.Errorf("Locate(%q) = false, want true", name)
			continue
		}
		if ext.Size != int64(len(want)) {
			t.Errorf("Locate(%q).Size = %d, want %d", name, ext.Size, len(want))
		}
		got, err := io.ReadAll(io.NewSectionReader(bytes.NewReader(raw), ext.Offset, ext.Size))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("Locate(%q): bytes at extent differ from entry content (err=%v)", name, err)
		}
	}
	for _, name := range []string{"deflated.txt", "dir/", "crypt.bin", "dup.bin", "missing.bin"} {
		if ext, ok := loc.Locate(name); ok {
			t.Errorf("Locate(%q) = %+v, true; want false", name, ext)
		}
	}
}

func TestZipLocate_TruncatedArchiveIsNotAddressable(t *testing.T) {
	data := noiseBytes(50_000, 3)
	p := writeZipSpecs(t, []zipSpec{{name: "a.bin", method: zip.Store, data: data}})
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the entry's uncompressed+compressed size in both headers so the
	// extent claims more bytes than the file holds: Locate must refuse.
	huge := []byte{0xff, 0xff, 0xff, 0x7f}
	local := bytes.Index(raw, []byte("PK\x03\x04"))
	central := bytes.Index(raw, []byte("PK\x01\x02"))
	copy(raw[local+18:], huge)   // local header: compressed size
	copy(raw[local+22:], huge)   // local header: uncompressed size
	copy(raw[central+20:], huge) // central dir: compressed size
	copy(raw[central+24:], huge) // central dir: uncompressed size
	bad := filepath.Join(t.TempDir(), "bad.zip")
	if err := os.WriteFile(bad, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, loc := mustLocator(t, bad, "zip")
	if ext, ok := loc.Locate("a.bin"); ok {
		t.Fatalf("Locate on oversize entry = %+v, true; want false", ext)
	}
}

func plainTar(t *testing.T, hdrs []*tar.Header, bodies [][]byte, gz bool) string {
	t.Helper()
	name := "locate.tar"
	if gz {
		name = "locate.tar.gz"
	}
	p := filepath.Join(t.TempDir(), name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	var w io.Writer = f
	var gw *gzip.Writer
	if gz {
		gw = gzip.NewWriter(f)
		w = gw
	}
	tw := tar.NewWriter(w)
	for i, h := range hdrs {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(bodies[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if gw != nil {
		if err := gw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func tarFixture() ([]*tar.Header, [][]byte) {
	long := strings.Repeat("deep/", 70) + "long-name.mkv" // forces a PAX header
	a, b, c := noiseBytes(70_000, 4), noiseBytes(513, 5), noiseBytes(1024, 6)
	hdrs := []*tar.Header{
		{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "a.mkv", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(a))},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "a.mkv", Mode: 0o644},
		{Name: long, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(b)), Format: tar.FormatPAX},
		{Name: "dup.bin", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(c))},
		{Name: "dup.bin", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(a))},
	}
	return hdrs, [][]byte{nil, a, nil, b, c, a}
}

func TestTarLocateAndOpenInPlace(t *testing.T) {
	hdrs, bodies := tarFixture()
	p := plainTar(t, hdrs, bodies, false)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	r, loc := mustLocator(t, p, "tar")

	for i, h := range hdrs {
		if h.Typeflag != tar.TypeReg || i == 5 { // i==5: shadowed duplicate
			continue
		}
		ext, ok := loc.Locate(h.Name)
		if !ok {
			t.Errorf("Locate(%.30q) = false, want true", h.Name)
			continue
		}
		if got := raw[ext.Offset : ext.Offset+ext.Size]; !bytes.Equal(got, bodies[i]) {
			t.Errorf("Locate(%.30q): extent bytes differ from entry content", h.Name)
		}
		// After the scan, Open serves the same bytes straight from the file.
		rc, err := r.Open(h.Name)
		if err != nil {
			t.Fatalf("Open(%.30q): %v", h.Name, err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil || !bytes.Equal(got, bodies[i]) {
			t.Errorf("Open(%.30q) after scan: content mismatch (err=%v)", h.Name, err)
		}
	}
	for _, name := range []string{"d/", "link", "nope"} {
		if ext, ok := loc.Locate(name); ok {
			t.Errorf("Locate(%q) = %+v, true; want false", name, ext)
		}
	}
}

func TestTarOpenWithoutScanStillSequential(t *testing.T) {
	hdrs, bodies := tarFixture()
	p := plainTar(t, hdrs, bodies, false)
	r, err := archive.OpenFile(p, "tar")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	rc, err := r.Open("a.mkv") // no List/Locate yet → walks headers
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil || !bytes.Equal(got, bodies[1]) {
		t.Fatalf("sequential Open mismatch (err=%v)", err)
	}
}

func TestTgzIsNotLocatable(t *testing.T) {
	hdrs, bodies := tarFixture()
	p := plainTar(t, hdrs, bodies, true)
	_, loc := mustLocator(t, p, "tgz")
	if ext, ok := loc.Locate("a.mkv"); ok {
		t.Fatalf("gzip tar entry must not be addressable in place, got %+v", ext)
	}
}

// TestListIsMemoized deletes the archive after the first List: a second List on
// the same Reader must be served from memory (no rescan, no re-decompression).
func TestListIsMemoized(t *testing.T) {
	hdrs, bodies := tarFixture()
	for _, ext := range []string{"tar", "tgz"} {
		t.Run(ext, func(t *testing.T) {
			p := plainTar(t, hdrs, bodies, ext == "tgz")
			r, err := archive.OpenFile(p, ext)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			first, err := r.List()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			second, err := r.List()
			if err != nil {
				t.Fatalf("second List after file removal: %v (not memoized)", err)
			}
			if len(second) != len(first) || len(first) != len(hdrs) {
				t.Fatalf("List lengths: first=%d second=%d want %d", len(first), len(second), len(hdrs))
			}
			// The returned slice is a copy: mutating it must not poison the cache.
			second[0].Name = "mutated"
			third, _ := r.List()
			if third[0].Name == "mutated" {
				t.Fatal("List returned the memoized slice itself")
			}
		})
	}
}
