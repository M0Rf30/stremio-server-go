package nzb

import (
	"fmt"
	"io"
	"sort"
)

// nzbDefaultMaxBytes is the write-cap applied when the NZB declares no segment
// sizes (or their sum is non-positive). 50 GiB comfortably covers the largest
// legitimate Usenet releases while still preventing disk exhaustion from
// adversarial or corrupt NZB data.
const nzbDefaultMaxBytes = 50 << 30

// limitedWriter wraps an io.Writer and returns an error if the total bytes
// written would exceed limit. A limit of 0 disables the cap.
type limitedWriter struct {
	w       io.Writer
	limit   int64
	written int64
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	if lw.limit > 0 {
		if lw.written >= lw.limit {
			return 0, fmt.Errorf("nzb: assembled output exceeds declared size %d bytes", lw.limit)
		}
		remain := lw.limit - lw.written
		if int64(len(p)) > remain {
			// Write only up to the limit, then return an overflow error.
			n, err := lw.w.Write(p[:remain])
			lw.written += int64(n)
			if err != nil {
				return n, err
			}
			return n, fmt.Errorf("nzb: assembled output exceeds declared size %d bytes", lw.limit)
		}
	}
	n, err := lw.w.Write(p)
	lw.written += int64(n)
	return n, err
}

// Session holds the NNTP server configuration and the parsed file list for one
// NZB job. It dials a fresh connection for each AssembleFile call, which keeps
// the implementation simple and avoids idle-timeout issues.
type Session struct {
	cfg   ServerConfig
	files []File
}

// NewSession creates a Session from server configuration and pre-parsed files.
func NewSession(cfg ServerConfig, files []File) *Session {
	return &Session{cfg: cfg, files: files}
}

// Files returns the files contained in this NZB job.
func (sess *Session) Files() []File {
	return sess.files
}

// AssembleFile downloads all segments for the file named name in order and
// writes the decoded bytes to dst. Segments are fetched sequentially over a
// single NNTP connection; the connection is closed when done.
// The total bytes written is capped at the declared file size to prevent disk
// exhaustion from malformed or adversarial NZB data.
func (sess *Session) AssembleFile(name string, dst io.Writer) error {
	var target *File
	for i := range sess.files {
		if sess.files[i].Name == name {
			target = &sess.files[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("nzb: file %q not found in NZB", name)
	}

	c, err := Dial(sess.cfg)
	if err != nil {
		return fmt.Errorf("nzb: %w", err)
	}
	defer func() { _ = c.Close() }()

	// Defensive copy sorted by segment number.
	segs := make([]Segment, len(target.Segments))
	copy(segs, target.Segments)
	sort.Slice(segs, func(i, j int) bool {
		return segs[i].Number < segs[j].Number
	})

	// Always wrap dst with a size cap to prevent disk exhaustion.
	// When the NZB omits or zeroes segment sizes (target.Size ≤ 0), fall back
	// to nzbDefaultMaxBytes so the cap is never disabled by missing metadata.
	capBytes := target.Size
	if capBytes <= 0 {
		capBytes = nzbDefaultMaxBytes
	}
	w := &limitedWriter{w: dst, limit: capBytes}

	for _, seg := range segs {
		if err := c.Body(seg.MessageID, w); err != nil {
			return fmt.Errorf("nzb: segment %s: %w", seg.MessageID, err)
		}
	}
	return nil
}
