package archive_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"strings"
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

// TestNormNamePathTraversal verifies that List() never returns entry names that
// could cause path traversal (absolute paths or names starting with "..").
func TestNormNamePathTraversal(t *testing.T) {
	// Build a zip whose entries have dangerous raw names.
	f, err := os.CreateTemp(t.TempDir(), "traversal-*.zip")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	zw := zip.NewWriter(f)
	dangerous := []string{
		"../secret.txt",
		"../../etc/passwd",
		"/etc/shadow",
		"safe/file.txt",
		"subdir/../also-safe.txt", // path.Clean resolves this to "also-safe.txt"
	}
	for _, name := range dangerous {
		fw, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte("data")); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := archive.OpenFile(f.Name(), "zip")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = r.Close() }()

	entries, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	for _, e := range entries {
		if strings.HasPrefix(e.Name, "/") {
			t.Errorf("entry %q: absolute path leaked through normName", e.Name)
		}
		for _, comp := range strings.Split(e.Name, "/") {
			if comp == ".." {
				t.Errorf("entry %q: contains '..' path component", e.Name)
			}
		}
	}
}

// makeTarRaw writes a tar archive with caller-supplied headers and data bodies.
// len(hdrs) must equal len(bodies); a nil body writes zero bytes (for symlinks,
// dirs, and empty files). Only the file is synced; the caller owns cleanup via
// t.TempDir().
func makeTarRaw(t *testing.T, hdrs []*tar.Header, bodies [][]byte) string {
	t.Helper()
	if len(hdrs) != len(bodies) {
		t.Fatalf("makeTarRaw: %d headers but %d bodies", len(hdrs), len(bodies))
	}
	f, err := os.CreateTemp(t.TempDir(), "raw-*.tar")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	tw := tar.NewWriter(f)
	for i, hdr := range hdrs {
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if len(bodies[i]) > 0 {
			if _, err := tw.Write(bodies[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

// TestTarTraversalNames builds a tar whose raw entry names include directory
// traversal sequences, absolute paths, and a symlink with a dangerous name.
// List must never expose a leading '/' or any '..' path component.
func TestTarTraversalNames(t *testing.T) {
	hdrs := []*tar.Header{
		// Regular files with traversal names.
		{Name: "../escape.txt", Typeflag: tar.TypeReg, Mode: 0644, Size: 4},
		{Name: "../../etc/passwd", Typeflag: tar.TypeReg, Mode: 0644, Size: 4},
		// Absolute path.
		{Name: "/absolute.txt", Typeflag: tar.TypeReg, Mode: 0644, Size: 4},
		// Symlink with dangerous name; Size must be 0 for symlinks.
		{Name: "../dangerous-link", Typeflag: tar.TypeSymlink, Mode: 0644, Linkname: "/etc/shadow", Size: 0},
		// Pure ".." directory: normName produces ".".
		{Name: "..", Typeflag: tar.TypeDir, Mode: 0755, Size: 0},
		// A safe entry that must survive unchanged.
		{Name: "safe/file.txt", Typeflag: tar.TypeReg, Mode: 0644, Size: 4},
	}
	bodies := [][]byte{
		[]byte("data"),
		[]byte("data"),
		[]byte("data"),
		nil,
		nil,
		[]byte("data"),
	}
	fpath := makeTarRaw(t, hdrs, bodies)

	r, err := archive.OpenFile(fpath, "tar")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = r.Close() }()

	entries, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	for _, e := range entries {
		if strings.HasPrefix(e.Name, "/") {
			t.Errorf("entry %q: absolute path leaked through normName", e.Name)
		}
		for _, comp := range strings.Split(e.Name, "/") {
			if comp == ".." {
				t.Errorf("entry %q: '..' component survived normName", e.Name)
			}
		}
	}
}

// TestTarWindowsBackslash verifies that backslash path separators found in
// Windows-style tar archives are normalised to forward slashes. Open() must
// also accept the normalised name.
func TestTarWindowsBackslash(t *testing.T) {
	const payload = "hello backslash"
	hdrs := []*tar.Header{
		{Name: `subdir\nested\file.txt`, Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(payload))},
	}
	bodies := [][]byte{[]byte(payload)}
	fpath := makeTarRaw(t, hdrs, bodies)

	r, err := archive.OpenFile(fpath, "tar")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = r.Close() }()

	entries, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("List: want 1 entry, got %d", len(entries))
	}
	const want = "subdir/nested/file.txt"
	if entries[0].Name != want {
		t.Errorf("Name: want %q, got %q", want, entries[0].Name)
	}

	rc, err := r.Open(want)
	if err != nil {
		t.Fatalf("Open by normalised name: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != payload {
		t.Errorf("content: want %q, got %q", payload, got)
	}
}

// TestTgzNestedDirs verifies that a tgz containing deeply nested paths round-
// trips through List and Open without corruption or name mangling.
func TestTgzNestedDirs(t *testing.T) {
	want := map[string][]byte{
		"a/b/c/deep.txt":  []byte("deep file content"),
		"a/b/sibling.txt": []byte("sibling content here"),
		"a/top.txt":       []byte("top level"),
		"root.txt":        []byte("root level"),
	}
	fpath := makeTgz(t, want)

	r, err := archive.OpenFile(fpath, "tgz")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = r.Close() }()

	entries, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
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
			t.Errorf("entry %q size: want %d, got %d", name, len(data), sz)
		}
	}

	// Open a deeply nested file and verify its bytes.
	rc, err := r.Open("a/b/c/deep.txt")
	if err != nil {
		t.Fatalf("Open nested entry: %v", err)
	}
	defer func() { _ = rc.Close() }()
	content, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(content, want["a/b/c/deep.txt"]) {
		t.Errorf("nested Open: got %q, want %q", content, want["a/b/c/deep.txt"])
	}
}

// TestEmptyZip verifies that an empty zip archive returns a non-error, zero-
// length list.
func TestEmptyZip(t *testing.T) {
	fpath := makeZip(t, nil)
	r, err := archive.OpenFile(fpath, "zip")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = r.Close() }()

	entries, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("empty zip: want 0 entries, got %d", len(entries))
	}
}

// TestEmptyTar verifies that an empty tar archive returns a non-error, zero-
// length list. This also covers the openTar success path (file readable).
func TestEmptyTar(t *testing.T) {
	fpath := makeTarRaw(t, nil, nil)
	r, err := archive.OpenFile(fpath, "tar")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = r.Close() }()

	entries, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("empty tar: want 0 entries, got %d", len(entries))
	}
}

// TestEmptyTgz verifies that an empty tgz archive returns a non-error, zero-
// length list.
func TestEmptyTgz(t *testing.T) {
	fpath := makeTgz(t, nil)
	r, err := archive.OpenFile(fpath, "tgz")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = r.Close() }()

	entries, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("empty tgz: want 0 entries, got %d", len(entries))
	}
}

// TestZipOpenNotFound verifies that Open returns an error when the requested
// entry name does not exist in a zip archive.
func TestZipOpenNotFound(t *testing.T) {
	fpath := makeZip(t, map[string][]byte{"exists.txt": []byte("here")})
	r, err := archive.OpenFile(fpath, "zip")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = r.Close() }()

	_, err = r.Open("no-such-file.txt")
	if err == nil {
		t.Error("Open(nonexistent): expected error, got nil")
	}
}

// TestTarOpenNotFound verifies that Open returns an error when the requested
// entry name does not exist in a tar/tgz archive. Uses tgz to exercise the
// gzip code path.
func TestTarOpenNotFound(t *testing.T) {
	fpath := makeTgz(t, map[string][]byte{"exists.txt": []byte("here")})
	r, err := archive.OpenFile(fpath, "tgz")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = r.Close() }()

	_, err = r.Open("no-such-file.txt")
	if err == nil {
		t.Error("Open(nonexistent) in tgz: expected error, got nil")
	}
}

// TestTarOpenNotFoundPlain verifies the same for a plain (non-gzip) tar.
func TestTarOpenNotFoundPlain(t *testing.T) {
	fpath := makeTarRaw(t,
		[]*tar.Header{{Name: "exists.txt", Typeflag: tar.TypeReg, Mode: 0644, Size: 4}},
		[][]byte{[]byte("here")},
	)
	r, err := archive.OpenFile(fpath, "tar")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = r.Close() }()

	_, err = r.Open("no-such-file.txt")
	if err == nil {
		t.Error("Open(nonexistent) in tar: expected error, got nil")
	}
}

// TestZipOpenStreamsFullBytes asserts that archive.zipReader.Open returns all
// actual entry bytes without truncation. Any zip-bomb size cap lives in the
// caller (api/archive.go), not in this package. If someone accidentally adds a
// size limit here, this test breaks.
func TestZipOpenStreamsFullBytes(t *testing.T) {
	payload := bytes.Repeat([]byte("Z"), 512)
	fpath := makeZip(t, map[string][]byte{"big.bin": payload})

	r, err := archive.OpenFile(fpath, "zip")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = r.Close() }()

	entries, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("List: want 1, got %d", len(entries))
	}
	if entries[0].Size != int64(len(payload)) {
		t.Errorf("List size: want %d, got %d", len(payload), entries[0].Size)
	}

	rc, err := r.Open("big.bin")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("Open: got %d bytes, want %d; bomb guard must not truncate here", len(got), len(payload))
	}
}
