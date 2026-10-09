// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package archive

import (
	"errors"
	"io"
	"os"
	"sync"
	"time"
)

// ErrChanged is returned when an archive file that is read in pieces (see
// OpenReaderAt) was replaced or modified between two of its reads: its size or
// modification time no longer match what they were when it was opened. The
// entry offsets learned from the old file would point into the new one, so the
// read fails instead of returning bytes from the wrong file.
var ErrChanged = errors.New("archive: file changed while it was being read")

// Read-block sizes of fileAt. A read that does not continue the previous one
// fetches reopenMinBlock bytes; every consecutive refill doubles the block up to
// reopenMaxBlock, so sequential streaming amortizes the open/close over large
// reads while a header or a directory probe never over-reads.
const (
	reopenMinBlock = 4 << 10
	reopenMaxBlock = 256 << 10
)

// OpenReaderAt returns an io.ReaderAt over the file at path that keeps no OS
// file handle between reads: every refill of its small read-ahead block opens
// the file, reads, and closes it again. It is what lets a Reader (zip, tar,
// tgz), an extraction or a checksum pass run in the background without holding
// the user's archive open: on Windows an open handle stops the file from being
// deleted or replaced, and a downloaded temp archive from being removed.
//
// The file must exist and be readable (checked now). Every later open verifies
// the size and modification time recorded now and fails with ErrChanged when
// they differ. Safe for concurrent use.
func OpenReaderAt(path string) (io.ReaderAt, error) {
	r, err := newFileAt(path)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// fileAt implements OpenReaderAt.
type fileAt struct {
	path  string
	size  int64
	mtime time.Time

	mu  sync.Mutex
	buf []byte // read-ahead block: file bytes [off, off+len(buf))
	off int64
}

func newFileAt(path string) (*fileAt, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return &fileAt{path: path, size: fi.Size(), mtime: fi.ModTime()}, nil
}

// ReadAt implements io.ReaderAt with os.File's semantics: a short read reports
// io.EOF and reads at or past the end return 0, io.EOF.
func (r *fileAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("archive: negative read offset")
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= r.size {
		return 0, io.EOF
	}
	var eof error
	if rem := r.size - off; int64(len(p)) > rem {
		p, eof = p[:rem], io.EOF
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for n < len(p) {
		pos := off + int64(n)
		if pos >= r.off && pos < r.off+int64(len(r.buf)) {
			n += copy(p[n:], r.buf[pos-r.off:])
			continue
		}
		if len(p)-n >= reopenMaxBlock {
			// Large reads (checksum passes) go straight into the caller's
			// buffer; caching them would only add a copy.
			if err := r.readFrom(p[n:], pos); err != nil {
				return n, err
			}
			return len(p), eof
		}
		if err := r.refill(pos, len(p)-n); err != nil {
			return n, err
		}
	}
	return n, eof
}

// refill replaces the read-ahead block with the one starting at pos, at least
// need bytes long. A refill that continues exactly where the block ended is
// sequential access and doubles the block size.
func (r *fileAt) refill(pos int64, need int) error {
	size := reopenMinBlock
	if pos == r.off+int64(len(r.buf)) && len(r.buf) > 0 {
		size = min(2*len(r.buf), reopenMaxBlock)
	}
	size = max(size, need)
	if rem := r.size - pos; int64(size) > rem {
		size = int(rem)
	}
	if cap(r.buf) < size {
		c := size
		if size > reopenMinBlock { // sequential: allocate the largest block once
			c = int(min(int64(reopenMaxBlock), r.size))
		}
		r.buf = make([]byte, 0, c)
	}
	r.buf = r.buf[:size]
	r.off = pos
	if err := r.readFrom(r.buf, pos); err != nil {
		r.buf = r.buf[:0]
		return err
	}
	return nil
}

// readFrom fills dst with the file's bytes at pos through a handle that lives
// only for this call.
func (r *fileAt) readFrom(dst []byte, pos int64) error {
	f, err := os.Open(r.path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() != r.size || !fi.ModTime().Equal(r.mtime) {
		return ErrChanged
	}
	n, err := f.ReadAt(dst, pos)
	if n == len(dst) {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return err
}
