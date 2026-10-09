// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"io"
	"testing"
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
