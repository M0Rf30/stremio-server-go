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

// deadlineRW is an http.ResponseWriter that records SetWriteDeadline calls.
type deadlineRW struct {
	discardRW
	deadlines []time.Time
	writes    int
}

func (d *deadlineRW) Write(p []byte) (int, error) {
	d.writes++
	return len(p), nil
}

func (d *deadlineRW) SetWriteDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
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
	// one deadline per write + the final clear
	if len(rw.deadlines) != rw.writes+1 {
		t.Fatalf("deadline calls = %d, want %d", len(rw.deadlines), rw.writes+1)
	}
	for i, d := range rw.deadlines[:rw.writes] {
		if d.IsZero() || time.Until(d) <= 0 || time.Until(d) > time.Minute {
			t.Errorf("deadline[%d] = %v, want within the next minute", i, d)
		}
	}
	if last := rw.deadlines[len(rw.deadlines)-1]; !last.IsZero() {
		t.Errorf("final deadline = %v, want cleared", last)
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
// cut the stream because the deadline is re-armed per write.
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
