// Package archive provides a uniform streaming reader over local archive files
// (zip, tar, tgz, rar, 7zip). All implementations are pure Go; no cgo or
// external binaries are required.
package archive

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	rardecode "github.com/nwaples/rardecode/v2"
)

// Entry describes a single item inside an archive. Directories may appear in
// the list; callers filter them by checking IsDir.
type Entry struct {
	Name  string // forward-slash separated relative path
	Size  int64
	IsDir bool
}

// Reader provides sequential-safe access to an archive's contents. Close must
// be called when the Reader is no longer needed to release underlying resources.
type Reader interface {
	// List returns all entries recorded in the archive (including directories).
	List() ([]Entry, error)
	// Open returns a sequential ReadCloser for the named entry. The name must
	// match an entry name returned by List exactly.
	Open(name string) (io.ReadCloser, error)
	// Close releases the Reader's resources.
	Close() error
}

// OpenFile opens the archive at fpath and returns a Reader for its contents.
// ext must be one of: "zip", "rar", "7zip", "tar", "tgz".
func OpenFile(fpath, ext string) (Reader, error) {
	switch ext {
	case "zip":
		return openZip(fpath)
	case "rar":
		return openRar(fpath)
	case "7zip":
		return openSevenZip(fpath)
	case "tar":
		return openTar(fpath)
	case "tgz":
		return openTgz(fpath)
	default:
		return nil, fmt.Errorf("archive: unsupported format %q", ext)
	}
}

// normName converts an archive entry name to a clean forward-slash path.
// It replaces back-slashes (Windows archives), collapses redundant separators
// and dot-segments, strips leading slashes (absolute → relative), and removes
// any leading ../ sequences that survive cleaning so that a malicious archive
// cannot cause path traversal outside the extraction directory.
func normName(s string) string {
	s = path.Clean(strings.ReplaceAll(s, "\\", "/"))
	// Absolute path → strip leading slashes.
	s = strings.TrimLeft(s, "/")
	// Strip any remaining leading ../ segments (e.g. "../../evil" becomes "evil").
	for s == ".." || strings.HasPrefix(s, "../") {
		if s == ".." {
			s = "."
			break
		}
		s = s[3:]
	}
	if s == "" {
		s = "."
	}
	return s
}

// ── zip ──────────────────────────────────────────────────────────────────────

type zipReader struct {
	rc    *zip.ReadCloser
	index map[string]*zip.File // normName → file; built once in openZip for O(1) Open
}

func openZip(fpath string) (Reader, error) {
	rc, err := zip.OpenReader(fpath)
	if err != nil {
		return nil, err
	}
	// Build index once; first-wins matches the previous linear-scan behaviour.
	idx := make(map[string]*zip.File, len(rc.File))
	for _, f := range rc.File {
		if n := normName(f.Name); n != "" {
			if _, ok := idx[n]; !ok {
				idx[n] = f
			}
		}
	}
	return &zipReader{rc: rc, index: idx}, nil
}

func (r *zipReader) List() ([]Entry, error) {
	out := make([]Entry, 0, len(r.rc.File))
	for _, f := range r.rc.File {
		fi := f.FileInfo()
		out = append(out, Entry{
			Name:  normName(f.Name),
			Size:  fi.Size(),
			IsDir: fi.IsDir(),
		})
	}
	return out, nil
}

func (r *zipReader) Open(name string) (io.ReadCloser, error) {
	// O(1) index lookup built during openZip; avoids O(N×normName) per call.
	if f, ok := r.index[name]; ok {
		return f.Open()
	}
	return nil, fmt.Errorf("archive: %q not found in zip", name)
}

func (r *zipReader) Close() error { return r.rc.Close() }

// ── tar / tgz ────────────────────────────────────────────────────────────────

// tarReader re-opens the underlying file on every List/Open call so that
// multiple sequential entries can be accessed without state carried between
// calls. Close is a no-op since no long-lived file handle is kept.
type tarReader struct {
	fpath string
	gz    bool // true → decompress with gzip before feeding to tar
}

func openTar(fpath string) (Reader, error) {
	f, err := os.Open(fpath)
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	return &tarReader{fpath: fpath}, nil
}

func openTgz(fpath string) (Reader, error) {
	f, err := os.Open(fpath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	gr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("archive: not a valid gzip stream: %w", err)
	}
	_ = gr.Close()
	return &tarReader{fpath: fpath, gz: true}, nil
}

// seqCloser closes a stack of io.Closers in LIFO order.
type seqCloser struct {
	closers []io.Closer
}

func (sc *seqCloser) Close() error {
	var last error
	for i := len(sc.closers) - 1; i >= 0; i-- {
		if err := sc.closers[i].Close(); err != nil {
			last = err
		}
	}
	return last
}

// openStream opens the underlying file (and optional gzip layer) and returns a
// *tar.Reader positioned at the start of the archive.
func (r *tarReader) openStream() (*tar.Reader, io.Closer, error) {
	f, err := os.Open(r.fpath)
	if err != nil {
		return nil, nil, err
	}
	if r.gz {
		gr, err := gzip.NewReader(f)
		if err != nil {
			_ = f.Close()
			return nil, nil, err
		}
		return tar.NewReader(gr), &seqCloser{closers: []io.Closer{gr, f}}, nil
	}
	return tar.NewReader(f), f, nil
}

func (r *tarReader) List() ([]Entry, error) {
	tr, cl, err := r.openStream()
	if err != nil {
		return nil, err
	}
	defer func() { _ = cl.Close() }()

	var out []Entry
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		out = append(out, Entry{
			Name:  normName(hdr.Name),
			Size:  hdr.Size,
			IsDir: hdr.FileInfo().IsDir(),
		})
	}
	return out, nil
}

// tarEntryReader wraps a tar.Reader positioned at a specific entry and closes
// the underlying file stack when Close is called.
type tarEntryReader struct {
	io.Reader
	closer io.Closer
}

func (t *tarEntryReader) Close() error { return t.closer.Close() }

func (r *tarReader) Open(name string) (io.ReadCloser, error) {
	tr, cl, err := r.openStream()
	if err != nil {
		return nil, err
	}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			_ = cl.Close()
			return nil, fmt.Errorf("archive: %q not found in tar", name)
		}
		if err != nil {
			_ = cl.Close()
			return nil, err
		}
		if normName(hdr.Name) == name {
			return &tarEntryReader{Reader: tr, closer: cl}, nil
		}
	}
}

func (r *tarReader) Close() error { return nil }

// ── rar ──────────────────────────────────────────────────────────────────────

// rarReader stores only the path; it re-opens a fresh ReadCloser for every
// List and Open call because rardecode is inherently sequential.
type rarReader struct {
	fpath string
}

func openRar(fpath string) (Reader, error) {
	// Validate the file is a readable RAR archive.
	rc, err := rardecode.OpenReader(fpath)
	if err != nil {
		return nil, err
	}
	_ = rc.Close()
	return &rarReader{fpath: fpath}, nil
}

func (r *rarReader) List() ([]Entry, error) {
	rc, err := rardecode.OpenReader(r.fpath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	var out []Entry
	for {
		hdr, err := rc.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		out = append(out, Entry{
			Name:  normName(hdr.Name),
			Size:  hdr.UnPackedSize,
			IsDir: hdr.IsDir,
		})
	}
	return out, nil
}

// rarEntryReader reads from a rardecode.ReadCloser positioned at the matched
// entry and closes the ReadCloser when Close is called.
type rarEntryReader struct {
	rc *rardecode.ReadCloser
}

func (r *rarEntryReader) Read(p []byte) (int, error) { return r.rc.Read(p) }
func (r *rarEntryReader) Close() error               { return r.rc.Close() }

func (r *rarReader) Open(name string) (io.ReadCloser, error) {
	rc, err := rardecode.OpenReader(r.fpath)
	if err != nil {
		return nil, err
	}
	for {
		hdr, err := rc.Next()
		if errors.Is(err, io.EOF) {
			_ = rc.Close()
			return nil, fmt.Errorf("archive: %q not found in rar", name)
		}
		if err != nil {
			_ = rc.Close()
			return nil, err
		}
		if normName(hdr.Name) == name {
			// rc is positioned to stream this entry's bytes.
			return &rarEntryReader{rc: rc}, nil
		}
		// rardecode.Reader.Next() discards remaining bytes of the current
		// entry automatically before advancing; no manual drain needed.
	}
}

func (r *rarReader) Close() error { return nil }
