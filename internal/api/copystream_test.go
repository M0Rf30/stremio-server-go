// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// readerFromWriter mimics *http.response: it implements io.ReaderFrom, which
// io.CopyBuffer would prefer over the caller's buffer.
type readerFromWriter struct {
	bytes.Buffer
	readFromCalled bool
}

func (w *readerFromWriter) ReadFrom(r io.Reader) (int64, error) {
	w.readFromCalled = true
	return w.Buffer.ReadFrom(r)
}

// maxReadReader records the largest Read the copy loop asked for.
type maxReadReader struct {
	r   io.Reader
	max int
}

func (m *maxReadReader) Read(p []byte) (int, error) {
	m.max = max(m.max, len(p))
	return m.r.Read(p)
}

func TestCopyStreamUsesPooledBuffer(t *testing.T) {
	src := bytes.Repeat([]byte("x"), 1<<20)
	dst := &readerFromWriter{}
	rr := &maxReadReader{r: bytes.NewReader(src)}
	bufp := streamBufPool.Get().(*[]byte)
	defer streamBufPool.Put(bufp)

	copyStream(dst, rr, *bufp)

	if dst.readFromCalled {
		t.Error("copyStream went through the writer's ReadFrom (pooled buffer bypassed)")
	}
	if rr.max != len(*bufp) {
		t.Errorf("largest read = %d bytes, want the pooled buffer size %d", rr.max, len(*bufp))
	}
	if !bytes.Equal(dst.Bytes(), src) {
		t.Error("copied data mismatch")
	}
}

// deadlineRW is an http.ResponseWriter that records SetWriteDeadline calls and
// the deadline that was in force at each Write.
type deadlineRW struct {
	discardRW
	deadlines []time.Time // every SetWriteDeadline argument, in order
	cur       time.Time   // deadline currently pending (zero: none)
	armed     []time.Time // pending deadline observed at each Write
	writes    int
}

func (d *deadlineRW) Write(p []byte) (int, error) {
	d.writes++
	d.armed = append(d.armed, d.cur)
	return len(p), nil
}

func (d *deadlineRW) SetWriteDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	d.cur = t
	return nil
}

func TestCopyStreamRearmsWriteDeadlinePerWrite(t *testing.T) {
	old := streamWriteIdleTimeout
	streamWriteIdleTimeout = time.Minute
	t.Cleanup(func() { streamWriteIdleTimeout = old })

	buf := make([]byte, 1024)
	rw := &deadlineRW{}
	copyStream(rw, &maxReadReader{r: bytes.NewReader(bytes.Repeat([]byte("x"), 4*1024))}, buf)

	if rw.writes != 4 {
		t.Fatalf("writes = %d, want 4", rw.writes)
	}
	// Each write is bracketed: armed right before it, cleared right after.
	if len(rw.deadlines) != 2*rw.writes {
		t.Fatalf("deadline calls = %d, want %d (arm+clear per write)", len(rw.deadlines), 2*rw.writes)
	}
	for i := range rw.writes {
		if d := rw.deadlines[2*i]; d.IsZero() || time.Until(d) <= 0 || time.Until(d) > time.Minute {
			t.Errorf("arm[%d] = %v, want within the next minute", i, d)
		}
		if !rw.deadlines[2*i+1].IsZero() {
			t.Errorf("clear[%d] = %v, want zero", i, rw.deadlines[2*i+1])
		}
		if rw.armed[i].IsZero() {
			t.Errorf("write %d ran with no deadline pending", i)
		}
	}
	if !rw.cur.IsZero() {
		t.Errorf("deadline left pending after copyStream: %v", rw.cur)
	}
}

// pendingProbeReader wraps a reader and records whether a write deadline is
// pending on rw at the moment the source is read.
type pendingProbeReader struct {
	r       io.Reader
	rw      *deadlineRW
	reads   int
	pending int
}

func (p *pendingProbeReader) Read(b []byte) (int, error) {
	p.reads++
	if !p.rw.cur.IsZero() {
		p.pending++
	}
	return p.r.Read(b)
}

// TestCopyStreamNoDeadlinePendingWhileReadingSource: the idle deadline is for
// a client that stops reading. While copyStream is blocked on the SOURCE (a
// torrent with no peers) no write is in flight, so no deadline may be pending
// — on HTTP/2 a pending deadline resets the stream even with no write queued.
func TestCopyStreamNoDeadlinePendingWhileReadingSource(t *testing.T) {
	old := streamWriteIdleTimeout
	streamWriteIdleTimeout = time.Minute
	t.Cleanup(func() { streamWriteIdleTimeout = old })

	rw := &deadlineRW{}
	src := &pendingProbeReader{r: bytes.NewReader(bytes.Repeat([]byte("x"), 4*1024)), rw: rw}
	copyStream(rw, src, make([]byte, 1024))

	if src.reads < 4 {
		t.Fatalf("source reads = %d, want >= 4", src.reads)
	}
	if src.pending != 0 {
		t.Errorf("a write deadline was pending during %d of %d source reads, want 0", src.pending, src.reads)
	}
}

func TestCopyStreamUnsupportedDeadlineStillCopies(t *testing.T) {
	rec := httptest.NewRecorder() // ResponseController: ErrNotSupported
	src := bytes.Repeat([]byte("y"), 3000)
	copyStream(rec, bytes.NewReader(src), make([]byte, 1024))
	if !bytes.Equal(rec.Body.Bytes(), src) {
		t.Error("copied data mismatch")
	}
}

// infiniteReader yields zero bytes forever.
type infiniteReader struct{}

func (infiniteReader) Read(p []byte) (int, error) { return len(p), nil }

// TestCopyStreamDropsStalledClient: a client that stops reading must release
// the handler once the idle write deadline passes.
func TestCopyStreamDropsStalledClient(t *testing.T) {
	old := streamWriteIdleTimeout
	streamWriteIdleTimeout = 200 * time.Millisecond
	t.Cleanup(func() { streamWriteIdleTimeout = old })

	finished := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer close(finished)
		buf := make([]byte, 256<<10)
		copyStream(w, infiniteReader{}, buf)
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	// never read the body: socket buffers fill, writes block, deadline fires.
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		t.Fatal("handler still blocked on a stalled client after the idle write deadline")
	}
}

// slowReader returns n chunks, sleeping between them (a slow upstream, not a
// slow client): total duration far exceeds the idle timeout, which must not
// cut the stream because a deadline is only pending during each write.
type slowReader struct {
	n     int
	pause time.Duration
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.n == 0 {
		return 0, io.EOF
	}
	s.n--
	time.Sleep(s.pause)
	return copy(p, "z"), nil
}

func TestCopyStreamLongStreamNotKilledByIdleDeadline(t *testing.T) {
	old := streamWriteIdleTimeout
	streamWriteIdleTimeout = 250 * time.Millisecond
	t.Cleanup(func() { streamWriteIdleTimeout = old })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		copyStream(w, &slowReader{n: 8, pause: 100 * time.Millisecond}, make([]byte, 16))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(body) != 8 {
		t.Errorf("body = %d bytes, want 8", len(body))
	}
}

// newHTTP2TestServer starts an HTTPS httptest server that negotiates HTTP/2
// (the production HTTPS listener's protocol); use srv.Client() to reach it.
func newHTTP2TestServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// stallSource yields head, then blocks for stall (a torrent with no peers: no
// bytes available, client healthy), then yields tail and EOF.
type stallSource struct {
	head, tail []byte
	stall      time.Duration
	step       int
}

func (s *stallSource) Read(p []byte) (int, error) {
	switch s.step {
	case 0:
		s.step++
		return copy(p, s.head), nil
	case 1:
		s.step++
		time.Sleep(s.stall)
		return copy(p, s.tail), nil
	}
	return 0, io.EOF
}

// TestCopyStreamSourceStallNotResetOnHTTP2: on HTTP/2 SetWriteDeadline arms a
// timer that resets the stream whether or not a write is pending, so a
// deadline left armed across a stalled SOURCE read killed healthy clients. The
// client here keeps reading throughout; only the source stalls (5x longer than
// the idle timeout), and the stream must survive it.
func TestCopyStreamSourceStallNotResetOnHTTP2(t *testing.T) {
	old := streamWriteIdleTimeout
	streamWriteIdleTimeout = 200 * time.Millisecond
	t.Cleanup(func() { streamWriteIdleTimeout = old })

	head := bytes.Repeat([]byte("h"), 32<<10) // > h2 handler buffer: flushed to the client
	tail := []byte("tail")
	srv := newHTTP2TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		copyStream(w, &stallSource{head: head, tail: tail, stall: time.Second}, make([]byte, 64<<10))
	}))

	resp, err := srv.Client().Get(srv.URL) //nolint:noctx // test
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ProtoMajor != 2 {
		t.Fatalf("proto = %s, want HTTP/2", resp.Proto)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read after %d bytes: %v (stream reset while only the source stalled)", len(body), err)
	}
	if want := len(head) + len(tail); len(body) != want {
		t.Errorf("body = %d bytes, want %d", len(body), want)
	}
}

// TestCopyStreamDropsStalledClientHTTP2: bracketing the deadline around each
// write must not lose the real protection — a client that stops reading is
// still dropped on HTTP/2 once its write stays blocked past the idle timeout.
func TestCopyStreamDropsStalledClientHTTP2(t *testing.T) {
	old := streamWriteIdleTimeout
	streamWriteIdleTimeout = 200 * time.Millisecond
	t.Cleanup(func() { streamWriteIdleTimeout = old })

	finished := make(chan struct{})
	srv := newHTTP2TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer close(finished)
		copyStream(w, infiniteReader{}, make([]byte, 256<<10))
	}))

	resp, err := srv.Client().Get(srv.URL) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ProtoMajor != 2 {
		t.Fatalf("proto = %s, want HTTP/2", resp.Proto)
	}
	// never read the body: the stream's flow-control window fills, the write
	// blocks, and the idle deadline must release the handler.
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		t.Fatal("handler still blocked on a stalled HTTP/2 client after the idle write deadline")
	}
}
