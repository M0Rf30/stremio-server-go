// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

// Package archive provides a uniform streaming reader over local archive files
// (zip, tar, tgz, rar, 7zip). All implementations are pure Go; no cgo or
// external binaries are required.
package archive

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"sync"

	rardecode "github.com/nwaples/rardecode/v2"
)

// Entry describes a single item inside an archive. Directories may appear in
// the list; callers filter them by checking IsDir.
type Entry struct {
	Name string // forward-slash separated relative path
	Size int64
	// SizeUnknown is true when the archive does not record the unpacked size
	// (streamed RARs). Size is then 0 and callers must extract until EOF.
	SizeUnknown bool
	IsDir       bool
}

// Reader provides sequential-safe access to an archive's contents. Close must
// be called when the Reader is no longer needed to release underlying resources.
type Reader interface {
	// List returns all entries recorded in the archive (including directories).
	// Implementations memoize the result, so repeated calls on one Reader do
	// not rescan (or, for tgz, re-decompress) the archive.
	List() ([]Entry, error)
	// Open returns a sequential ReadCloser for the named entry. The name must
	// match an entry name returned by List exactly.
	Open(name string) (io.ReadCloser, error)
	// Close releases the Reader's resources.
	Close() error
}

// Extent locates an entry's bytes verbatim (stored uncompressed and
// contiguous) inside the archive file itself, so they can be served with
// ReadAt/io.SectionReader straight from the archive without any extraction.
type Extent struct {
	Offset int64 // byte offset of the entry data within the archive file
	Size   int64 // length of the entry data in bytes
	// CRC32 is the IEEE CRC-32 the archive records for the entry's Size bytes
	// (zip). It is meaningful only when HasCRC is set; tar records none.
	CRC32  uint32
	HasCRC bool
}

// ErrChecksum is returned by Extent.Verify when the entry's bytes do not match
// the CRC-32 the archive records for them. It is zip.ErrChecksum, i.e. the same
// error the extraction path reports for a corrupt entry.
var ErrChecksum = zip.ErrChecksum

// verifyBufSize is the read size of Extent.Verify; the context is re-checked
// once per buffer.
const verifyBufSize = 256 << 10

// Verify hashes the extent's bytes through r (normally the archive file) and
// compares them with the recorded CRC-32. It returns nil when the archive
// records no checksum (HasCRC false), ErrChecksum on a mismatch,
// io.ErrUnexpectedEOF when r holds fewer than Size bytes at Offset, and
// ctx.Err() when cancelled. It is what makes serving a stored entry in place as
// safe as extracting it: zip's own reader performs the same check at EOF.
func (e Extent) Verify(ctx context.Context, r io.ReaderAt) error {
	if !e.HasCRC {
		return nil
	}
	h := crc32.NewIEEE()
	buf := make([]byte, verifyBufSize)
	var off int64
	for off < e.Size {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := int64(len(buf))
		if rem := e.Size - off; rem < n {
			n = rem
		}
		nr, err := r.ReadAt(buf[:n], e.Offset+off)
		_, _ = h.Write(buf[:nr])
		off += int64(nr)
		if int64(nr) < n { // ReadAt reports a short read with a non-nil error
			if err == nil || errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
	}
	if h.Sum32() != e.CRC32 {
		return ErrChecksum
	}
	return nil
}

// Locator is optionally implemented by Readers (zip, plain tar) that can
// report where an entry's raw bytes live in the archive file. Locate returns
// false when the entry is compressed, encrypted, sparse, a directory, absent,
// or otherwise not addressable in place; callers then fall back to Open. The
// Extent stays valid for as long as the archive file itself is unchanged and
// does not depend on the Reader staying open.
type Locator interface {
	Locate(name string) (Extent, bool)
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

// zipReader owns its *os.File (instead of using zip.OpenReader) so Locate can
// bounds-check entry extents against the real archive size.
type zipReader struct {
	f     *os.File
	size  int64
	zr    *zip.Reader
	index map[string]*zip.File // normName → file; built once in openZip for O(1) Open
}

func openZip(fpath string) (Reader, error) {
	f, err := os.Open(fpath)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	zr, err := zip.NewReader(f, st.Size())
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	// Build index once; first-wins matches the previous linear-scan behaviour.
	idx := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		if n := normName(f.Name); n != "" {
			if _, ok := idx[n]; !ok {
				idx[n] = f
			}
		}
	}
	return &zipReader{f: f, size: st.Size(), zr: zr, index: idx}, nil
}

func (r *zipReader) List() ([]Entry, error) {
	out := make([]Entry, 0, len(r.zr.File))
	for _, f := range r.zr.File {
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

// zipFlagsEncrypted covers the "encrypted" (0x1) and "strong encryption"
// (0x40) general-purpose bits; the stdlib cannot decrypt either.
const zipFlagsEncrypted = 0x1 | 0x40

// zipFlagDataDescriptor is the general-purpose bit announcing a trailing data
// descriptor (CRC-32 and sizes written after the data).
const zipFlagDataDescriptor = 0x8

// Locate reports the in-file extent of a stored (method 0), unencrypted entry.
// The extent carries the recorded CRC-32 so callers serving it in place can
// verify it (Extent.Verify), as f.Open does at EOF.
// Deflated entries have no addressable plaintext and return false.
func (r *zipReader) Locate(name string) (Extent, bool) {
	f, ok := r.index[name]
	if !ok || f.Method != zip.Store || f.Flags&zipFlagsEncrypted != 0 || f.FileInfo().IsDir() {
		return Extent{}, false
	}
	if f.CompressedSize64 != f.UncompressedSize64 || f.UncompressedSize64 > uint64(r.size) {
		return Extent{}, false
	}
	off, err := f.DataOffset()
	size := int64(f.UncompressedSize64)
	if err != nil || off < 0 || off > r.size || size > r.size-off {
		return Extent{}, false
	}
	// Mirror what zip.File.Open verifies at EOF: always with a data
	// descriptor, otherwise only when the header records a non-zero CRC-32.
	hasCRC := f.CRC32 != 0 || f.Flags&zipFlagDataDescriptor != 0
	return Extent{Offset: off, Size: size, CRC32: f.CRC32, HasCRC: hasCRC}, true
}

func (r *zipReader) Close() error { return r.f.Close() }

// ── tar / tgz ────────────────────────────────────────────────────────────────

// tarReader re-opens the underlying file on every Open call so that multiple
// sequential entries can be accessed without state carried between calls.
// Close is a no-op since no long-lived file handle is kept. The first
// List/Locate scans the archive once and memoizes the entry list; for a plain
// (non-gzip) tar it also records each regular entry's data offset, which makes
// later Open calls O(1) and lets callers serve entries in place (Locator).
type tarReader struct {
	fpath string
	gz    bool // true → decompress with gzip before feeding to tar

	mu      sync.Mutex
	scanned bool
	entries []Entry
	extents map[string]Extent // plain tar only: normName → data extent (first wins)
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

// tarInPlace reports whether hdr describes a regular file whose bytes are laid
// out contiguously right after its header(s). Sparse files (old GNU 'S' type or
// PAX GNU.sparse.* records) are excluded: tar.Reader synthesizes their holes.
func tarInPlace(hdr *tar.Header) bool {
	if hdr.Typeflag != tar.TypeReg {
		return false
	}
	for k := range hdr.PAXRecords {
		if strings.HasPrefix(k, "GNU.sparse.") {
			return false
		}
	}
	return true
}

// scan lists the archive once and memoizes the result. A failed scan is not
// cached so a later call can retry.
func (r *tarReader) scan() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.scanned {
		return nil
	}
	tr, cl, err := r.openStream()
	if err != nil {
		return err
	}
	defer func() { _ = cl.Close() }()

	// For a plain tar the closer is the *os.File itself; tar.Reader reads
	// header blocks without read-ahead, so the file offset right after Next()
	// is exactly the start of the entry data.
	var pos io.Seeker
	var extents map[string]Extent
	if !r.gz {
		if s, ok := cl.(io.Seeker); ok {
			pos, extents = s, make(map[string]Extent)
		}
	}
	seen := make(map[string]struct{})
	var out []Entry
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := normName(hdr.Name)
		out = append(out, Entry{
			Name:  name,
			Size:  hdr.Size,
			IsDir: hdr.FileInfo().IsDir(),
		})
		if _, dup := seen[name]; dup {
			continue // first wins, matching Open
		}
		seen[name] = struct{}{}
		if pos != nil && tarInPlace(hdr) {
			if off, err := pos.Seek(0, io.SeekCurrent); err == nil {
				extents[name] = Extent{Offset: off, Size: hdr.Size}
			}
		}
	}
	r.entries, r.extents, r.scanned = out, extents, true
	return nil
}

func (r *tarReader) List() ([]Entry, error) {
	if err := r.scan(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.entries), nil
}

// Locate reports the in-file extent of a regular entry of a plain tar. A
// gzip-compressed tar has no addressable plaintext and always returns false.
func (r *tarReader) Locate(name string) (Extent, bool) {
	if r.gz || r.scan() != nil {
		return Extent{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.extents[name]
	return e, ok
}

// tarEntryReader wraps a tar.Reader positioned at a specific entry and closes
// the underlying file stack when Close is called.
type tarEntryReader struct {
	io.Reader
	closer io.Closer
}

func (t *tarEntryReader) Close() error { return t.closer.Close() }

// sectionReadCloser is an io.SectionReader that closes its backing file.
type sectionReadCloser struct {
	*io.SectionReader
	closer io.Closer
}

func (s *sectionReadCloser) Close() error { return s.closer.Close() }

func (r *tarReader) Open(name string) (io.ReadCloser, error) {
	// Once the archive has been scanned, a plain tar entry is addressable in
	// place: skip the sequential walk and read the section directly.
	if ext, ok := r.cachedExtent(name); ok {
		f, err := os.Open(r.fpath)
		if err != nil {
			return nil, err
		}
		return &sectionReadCloser{SectionReader: io.NewSectionReader(f, ext.Offset, ext.Size), closer: f}, nil
	}
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

// cachedExtent returns the memoized extent for name without triggering a scan.
func (r *tarReader) cachedExtent(name string) (Extent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.scanned {
		return Extent{}, false
	}
	e, ok := r.extents[name]
	return e, ok
}

func (r *tarReader) Close() error { return nil }

// ── rar ──────────────────────────────────────────────────────────────────────

// rarReader stores only the path; it re-opens a fresh ReadCloser for every
// Open call because rardecode is inherently sequential. The entry list is
// scanned once and memoized.
type rarReader struct {
	fpath string

	mu      sync.Mutex
	listed  bool
	entries []Entry
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
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listed {
		return slices.Clone(r.entries), nil
	}
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
		size, unknown := hdr.UnPackedSize, hdr.UnKnownSize
		if size < 0 {
			size, unknown = 0, true
		}
		if unknown {
			size = 0
		}
		out = append(out, Entry{
			Name:        normName(hdr.Name),
			Size:        size,
			SizeUnknown: unknown,
			IsDir:       hdr.IsDir,
		})
	}
	r.entries, r.listed = out, true
	return slices.Clone(out), nil
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
