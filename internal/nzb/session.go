package nzb

import (
	"fmt"
	"io"
	"sort"
)

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
	defer c.Close()

	// Defensive copy sorted by segment number.
	segs := make([]Segment, len(target.Segments))
	copy(segs, target.Segments)
	sort.Slice(segs, func(i, j int) bool {
		return segs[i].Number < segs[j].Number
	})

	for _, seg := range segs {
		if err := c.Body(seg.MessageID, dst); err != nil {
			return fmt.Errorf("nzb: segment %s: %w", seg.MessageID, err)
		}
	}
	return nil
}
