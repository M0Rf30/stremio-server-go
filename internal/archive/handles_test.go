// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package archive_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/archive"
)

// A Reader (and the entry streams it hands out) must not hold an OS file handle
// between reads: on Windows an open handle stops the user from deleting or
// replacing their archive, and a downloaded temp archive from being removed.

const fdTable = "/proc/self/fd" // Linux only; the handle tests skip elsewhere

func requireFDTable(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(fdTable); err != nil {
		t.Skip("needs /proc/self/fd to observe open file handles")
	}
}

// openHandles lists the open file descriptors of this process that refer to path.
func openHandles(path string) []string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	ents, err := os.ReadDir(fdTable)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		target, err := os.Readlink(filepath.Join(fdTable, e.Name()))
		if err == nil && strings.TrimSuffix(target, " (deleted)") == path {
			out = append(out, target)
		}
	}
	return out
}

func expectNoHandles(t *testing.T, when, path string) {
	t.Helper()
	if hs := openHandles(path); len(hs) != 0 {
		t.Errorf("%s: archive file is held open (%d handle(s))", when, len(hs))
	}
}

// plainTarFile writes a tar with one entry of data and returns its path.
func plainTarFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "h.tar")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadersHoldNoHandleBetweenReads(t *testing.T) {
	requireFDTable(t)
	content := noiseBytes(700_000, 11)
	cases := []struct {
		name, ext string
		path      func(t *testing.T) string
		locatable bool
	}{
		{"zip stored", "zip", func(t *testing.T) string {
			return writeZipSpecs(t, []zipSpec{{name: "m.bin", method: zip.Store, data: content}})
		}, true},
		{"zip deflated", "zip", func(t *testing.T) string {
			return writeZipSpecs(t, []zipSpec{{name: "m.bin", method: zip.Deflate, data: content}})
		}, false},
		{"tar", "tar", func(t *testing.T) string { return plainTarFile(t, "m.bin", content) }, true},
		{"tgz", "tgz", func(t *testing.T) string { return makeTgz(t, map[string][]byte{"m.bin": content}) }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.path(t)
			r, err := archive.OpenFile(p, tc.ext)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			expectNoHandles(t, "after OpenFile", p)

			if _, err := r.List(); err != nil {
				t.Fatal(err)
			}
			expectNoHandles(t, "after List", p)
			if loc, ok := r.(archive.Locator); ok {
				_, found := loc.Locate("m.bin")
				if found != tc.locatable {
					t.Errorf("Locate found=%v, want %v", found, tc.locatable)
				}
				expectNoHandles(t, "after Locate", p)
			}

			rc, err := r.Open("m.bin")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rc.Close() }()
			expectNoHandles(t, "after Open", p)
			head := make([]byte, 100_000)
			if _, err := io.ReadFull(rc, head); err != nil {
				t.Fatal(err)
			}
			expectNoHandles(t, "mid-stream", p) // the entry is half read, not closed
			rest, err := io.ReadAll(rc)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(append(head, rest...), content) {
				t.Error("entry bytes differ")
			}
			expectNoHandles(t, "after the entry was read", p)
		})
	}
}

// The reader does not pin the file: it can be removed while the Reader (and its
// memoized listing) are alive, and what was cached keeps being served. A read
// that needs the file afterwards fails cleanly instead of panicking.
func TestZipReaderSurvivesRemovalOfItsFile(t *testing.T) {
	p := writeZipSpecs(t, []zipSpec{{name: "m.bin", method: zip.Deflate, data: noiseBytes(300_000, 12)}})
	r, err := archive.OpenFile(p, "zip")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if err := os.Remove(p); err != nil {
		t.Fatalf("an open Reader must not stop its file from being deleted: %v", err)
	}
	if es, err := r.List(); err != nil || len(es) != 1 {
		t.Fatalf("List after removal: %v, %v", es, err)
	}
	rc, err := r.Open("m.bin")
	if err == nil {
		_, err = io.ReadAll(rc)
		_ = rc.Close()
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("reading a vanished archive: err = %v, want fs.ErrNotExist", err)
	}
}

// A file replaced under a live Reader must not be read at offsets learned from
// the old one: the read fails with ErrChanged.
func TestZipReaderDetectsReplacedFile(t *testing.T) {
	p := writeZipSpecs(t, []zipSpec{{name: "m.bin", method: zip.Deflate, data: noiseBytes(300_000, 13)}})
	r, err := archive.OpenFile(p, "zip")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if err := os.WriteFile(p, noiseBytes(50_000, 14), 0o600); err != nil {
		t.Fatal(err)
	}
	rc, err := r.Open("m.bin")
	if err == nil {
		_, err = io.ReadAll(rc)
		_ = rc.Close()
	}
	if !errors.Is(err, archive.ErrChanged) {
		t.Errorf("err = %v, want archive.ErrChanged", err)
	}
}

func TestOpenReaderAt(t *testing.T) {
	data := noiseBytes(1_300_000, 15)
	p := filepath.Join(t.TempDir(), "r.bin")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	ra, err := archive.OpenReaderAt(p)
	if err != nil {
		t.Fatal(err)
	}
	ref := bytes.NewReader(data)

	check := func(off int64, n int) {
		t.Helper()
		got, want := make([]byte, n), make([]byte, n)
		gn, gerr := ra.ReadAt(got, off)
		wn, werr := ref.ReadAt(want, off)
		if gn != wn || !bytes.Equal(got[:gn], want[:wn]) || errors.Is(gerr, io.EOF) != errors.Is(werr, io.EOF) || (gerr == nil) != (werr == nil) {
			t.Fatalf("ReadAt(%d bytes @%d) = %d, %v; want %d, %v", n, off, gn, gerr, wn, werr)
		}
	}

	t.Run("sequential, small and large steps", func(t *testing.T) {
		for _, step := range []int{1, 7, 512, 4096, 33_000, 300_000} {
			for off := int64(0); off < int64(len(data)); off += int64(step) {
				check(off, step)
			}
		}
	})
	t.Run("random", func(t *testing.T) {
		rng := rand.New(rand.NewPCG(1, 2))
		for range 3000 {
			check(rng.Int64N(int64(len(data))+10), rng.IntN(70_000)+1)
		}
	})
	t.Run("edges", func(t *testing.T) {
		n := int64(len(data))
		for _, c := range []struct {
			off int64
			n   int
		}{{0, 0}, {n, 1}, {n - 1, 1}, {n - 1, 10}, {n + 100, 5}, {0, len(data)}, {0, len(data) + 50}, {n - 3, 262_144}} {
			check(c.off, c.n)
		}
		if _, err := ra.ReadAt(make([]byte, 1), -1); err == nil {
			t.Error("negative offset accepted")
		}
	})
	t.Run("concurrent", func(t *testing.T) {
		done := make(chan struct{})
		for i := range 4 {
			go func() {
				defer func() { done <- struct{}{} }()
				rng := rand.New(rand.NewPCG(uint64(i), 9))
				for range 300 {
					off, n := rng.Int64N(int64(len(data))), rng.IntN(40_000)+1
					got := make([]byte, n)
					gn, _ := ra.ReadAt(got, off)
					if !bytes.Equal(got[:gn], data[off:off+int64(gn)]) {
						t.Errorf("concurrent read @%d differs", off)
						return
					}
				}
			}()
		}
		for range 4 {
			<-done
		}
	})
}

func TestOpenReaderAt_MissingFileAndChangedFile(t *testing.T) {
	if _, err := archive.OpenReaderAt(filepath.Join(t.TempDir(), "nope")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: err = %v, want fs.ErrNotExist", err)
	}

	data := noiseBytes(1_000_000, 16)
	p := filepath.Join(t.TempDir(), "c.bin")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	ra, err := archive.OpenReaderAt(p)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10)
	if _, err := ra.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}

	// Same size, different mtime: no longer the file that was opened.
	if err := os.Chtimes(p, time.Now(), st.ModTime().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := ra.ReadAt(buf, 900_000); !errors.Is(err, archive.ErrChanged) {
		t.Errorf("changed mtime: err = %v, want ErrChanged", err)
	}
	// Restored: reads work again (identity is compared, nothing is latched).
	if err := os.Chtimes(p, time.Now(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := ra.ReadAt(buf, 900_000); err != nil {
		t.Errorf("after restoring the mtime: %v", err)
	}
	// Truncated: a shorter file is a different file, not a short read.
	if err := os.Truncate(p, 500_000); err != nil {
		t.Fatal(err)
	}
	if _, err := ra.ReadAt(buf, 100_000); !errors.Is(err, archive.ErrChanged) {
		t.Errorf("truncated file: err = %v, want ErrChanged", err)
	}
}
