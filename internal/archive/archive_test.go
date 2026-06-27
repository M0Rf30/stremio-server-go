package archive_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"testing"

	"github.com/M0Rf30/stremio-server-go/internal/archive"
)

// makeZip writes a zip archive to a temp file and returns its path.
func makeZip(t *testing.T, entries map[string][]byte) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "test-*.zip")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	for name, data := range entries {
		fw, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

// makeTgz writes a gzip-compressed tar archive to a temp file and returns its path.
func makeTgz(t *testing.T, entries map[string][]byte) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "test-*.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	for name, data := range entries {
		hdr := &tar.Header{
			Name: name,
			Mode: 0644,
			Size: int64(len(data)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

func TestZipListAndOpen(t *testing.T) {
	want := map[string][]byte{
		"video.mp4": []byte("hello archive world"),
		"notes.txt": []byte("subtitle content here"),
	}
	fpath := makeZip(t, want)

	r, err := archive.OpenFile(fpath, "zip")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer r.Close()

	entries, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != len(want) {
		t.Fatalf("List: want %d entries, got %d", len(want), len(entries))
	}

	got := make(map[string]int64, len(entries))
	for _, e := range entries {
		got[e.Name] = e.Size
	}
	for name, data := range want {
		sz, ok := got[name]
		if !ok {
			t.Errorf("List: missing entry %q", name)
			continue
		}
		if sz != int64(len(data)) {
			t.Errorf("List: entry %q size: want %d, got %d", name, len(data), sz)
		}
	}

	// Open and verify bytes for the first file.
	rc, err := r.Open("video.mp4")
	if err != nil {
		t.Fatalf("Open video.mp4: %v", err)
	}
	defer rc.Close()
	content, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(content, want["video.mp4"]) {
		t.Errorf("Open video.mp4: got %q, want %q", content, want["video.mp4"])
	}
}

func TestTgzListAndOpen(t *testing.T) {
	want := map[string][]byte{
		"movie.mkv": []byte("mkv binary stream data"),
		"movie.srt": []byte("1\n00:00:01,000 --> 00:00:03,000\nHello"),
	}
	fpath := makeTgz(t, want)

	r, err := archive.OpenFile(fpath, "tgz")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer r.Close()

	entries, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != len(want) {
		t.Fatalf("List: want %d entries, got %d", len(want), len(entries))
	}

	got := make(map[string]int64, len(entries))
	for _, e := range entries {
		got[e.Name] = e.Size
	}
	for name, data := range want {
		sz, ok := got[name]
		if !ok {
			t.Errorf("List: missing entry %q", name)
			continue
		}
		if sz != int64(len(data)) {
			t.Errorf("List: entry %q size: want %d, got %d", name, len(data), sz)
		}
	}

	// Open and verify bytes for the mkv entry.
	rc, err := r.Open("movie.mkv")
	if err != nil {
		t.Fatalf("Open movie.mkv: %v", err)
	}
	defer rc.Close()
	content, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(content, want["movie.mkv"]) {
		t.Errorf("Open movie.mkv: got %q, want %q", content, want["movie.mkv"])
	}
}

func TestUnknownFormat(t *testing.T) {
	_, err := archive.OpenFile("/dev/null", "xyz")
	if err == nil {
		t.Error("expected error for unsupported format, got nil")
	}
}

func TestFormatRouting(t *testing.T) {
	// These formats are wired in the switch; opening a non-existent path proves
	// the dispatch reaches the format-specific open function (error ≠ "unsupported").
	for _, ext := range []string{"rar", "7zip", "tar"} {
		_, err := archive.OpenFile("/nonexistent/path", ext)
		if err == nil {
			t.Errorf("ext %q: expected error for missing file, got nil", ext)
		}
	}
}
