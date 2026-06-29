package nzb

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
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
// NZB job. It maintains a persistent NNTP connection across AssembleFile calls
// to amortise the TCP+TLS+AUTHINFO handshake cost (100–400 ms) that would
// otherwise be paid on every range request in seekable NZB streaming.
// A mutex serialises access because Client is not safe for concurrent use.
type Session struct {
	cfg    ServerConfig
	files  []File
	mu     sync.Mutex // guards client
	client *Client    // persistent; lazily dialled; nil = not yet connected
}

// NewSession creates a Session from server configuration and pre-parsed files.
func NewSession(cfg ServerConfig, files []File) *Session {
	return &Session{cfg: cfg, files: files}
}

// Files returns the files contained in this NZB job.
func (sess *Session) Files() []File {
	return sess.files
}

// Close releases the persistent NNTP connection held by this Session, if any.
// It is safe to call Close concurrently with or after AssembleFile.
func (sess *Session) Close() error {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.client != nil {
		err := sess.client.Close()
		sess.client = nil
		return err
	}
	return nil
}

// isConnErr reports whether err indicates a broken or closed underlying TCP
// connection (EOF, broken pipe, timeout, connection reset) rather than an NNTP
// protocol-level error (e.g. 430 No Such Article). Used to gate reconnects.
func isConnErr(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne)
}

// AssembleFile downloads all segments for the file named name in order and
// writes the decoded bytes to dst. Segments are fetched sequentially over the
// Session's persistent NNTP connection.
//
// If the connection is found dead (server-side idle timeout, broken pipe), one
// transparent re-dial is attempted per segment before returning the error; this
// covers the common case where the server closes an idle connection between
// range requests. Re-dial is suppressed if any bytes of the segment were
// already delivered to dst — duplicating delivered bytes would corrupt output.
//
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

	// Always wrap dst with a size cap to prevent disk exhaustion.
	// When the NZB omits or zeroes segment sizes (target.Size ≤ 0), fall back
	// to nzbDefaultMaxBytes so the cap is never disabled by missing metadata.
	capBytes := target.Size
	if capBytes <= 0 {
		capBytes = nzbDefaultMaxBytes
	}
	w := &limitedWriter{w: dst, limit: capBytes}

	// Serialise NNTP command sequences: Client is not safe for concurrent use.
	sess.mu.Lock()
	defer sess.mu.Unlock()

	// Lazily dial the persistent connection on first use.
	if sess.client == nil {
		c, err := Dial(sess.cfg)
		if err != nil {
			return fmt.Errorf("nzb: %w", err)
		}
		sess.client = c
	}

	// nzb.Parse already emits segments sorted ascending by Number; the
	// defensive copy+re-sort that was here is redundant and has been removed.
	for _, seg := range target.Segments {
		// Track how many bytes have been delivered to dst before this segment
		// so we can decide whether a reconnect-and-retry is safe.
		writtenBefore := w.written

		err := sess.client.Body(seg.MessageID, w)
		if err == nil {
			continue
		}

		// Only attempt a transparent reconnect when:
		//   (a) the error looks like a dead connection (not a 430-style NNTP error), AND
		//   (b) no bytes of this segment have been written to dst yet.
		// If bytes were already delivered, retrying would duplicate them.
		if !isConnErr(err) || w.written != writtenBefore {
			return fmt.Errorf("nzb: segment %s: %w", seg.MessageID, err)
		}

		// Connection error before any segment data was delivered: discard the
		// dead client, re-dial once, and retry the same segment.
		_ = sess.client.Close()
		sess.client = nil
		c, dialErr := Dial(sess.cfg)
		if dialErr != nil {
			return fmt.Errorf("nzb: reconnect after connection error: %w", dialErr)
		}
		sess.client = c
		if err = sess.client.Body(seg.MessageID, w); err != nil {
			return fmt.Errorf("nzb: segment %s: %w", seg.MessageID, err)
		}
	}
	return nil
}
