// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package streamproxy

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/logging"
)

// A client that reads the response headers of a decrypted segment and then
// stops reading must not keep the segment's in-flight reservation: the proxy
// has no server-side WriteTimeout, so without a bounded write the reservation
// (up to maxSegmentBytes) would be pinned for as long as the client keeps the
// connection open, starving every other decrypt (503 after the 30 s wait) and
// every cache fill / prefetch (tryAcquire fails while a waiter is queued).
func TestServeStreamDecryptStalledClientDoesNotPinBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(http.ResponseWriter) http.ResponseWriter
	}{
		{"direct", func(w http.ResponseWriter) http.ResponseWriter { return w }},
		// The access-log wrapper used by the real server chain must stay
		// transparent to the per-write deadline (via Unwrap).
		{"logging wrapper", func(w http.ResponseWriter) http.ResponseWriter { return logging.NewResponseRecorder(w) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, iv := []byte("0123456789abcdef"), []byte("fedcba9876543210")
			// Far larger than the socket buffers on both sides of the loopback
			// connection, so an unread response is guaranteed to block the write.
			plain, ct := drmTestCBCSegment(t, key, iv, 24<<20)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", strconv.Itoa(len(ct)))
				_, _ = w.Write(ct)
			}))
			defer upstream.Close()

			h := newStreamProxyForTest(t, upstream, 0)
			h.writeIdle = 300 * time.Millisecond
			// Room for exactly one segment: a second decrypt can only start once
			// the first has released its reservation.
			h.inflight = newByteBudget(int64(len(ct)))
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				h.serveStream(tc.wrap(w), r)
			}))
			defer proxy.Close()
			target := decryptTarget(upstream.URL+"/s.ts", key, iv)

			// Stalled client: tiny receive buffer, reads the status line, then never
			// reads again.
			conn, err := net.Dial("tcp", proxy.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.SetReadBuffer(4 << 10)
			}
			if _, err := io.WriteString(conn, "GET "+target+" HTTP/1.1\r\nHost: proxy\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			status, err := bufio.NewReaderSize(conn, 256).ReadString('\n')
			if err != nil || !strings.Contains(status, " 200 ") {
				t.Fatalf("stalled client status line %q, err=%v", status, err)
			}

			// A healthy client must still get its segment while the stalled one
			// sits on its connection.
			client := &http.Client{Timeout: 10 * time.Second}
			resp, err := client.Get(proxy.URL + target)
			if err != nil {
				t.Fatalf("second decrypt request starved by a stalled client: %v", err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(body, plain) {
				t.Fatalf("second decrypt: status=%d err=%v body ok=%v", resp.StatusCode, err, bytes.Equal(body, plain))
			}

			// Both reservations must end up returned once the handlers finish.
			_ = conn.Close()
			deadline := time.Now().Add(5 * time.Second)
			for {
				h.inflight.mu.Lock()
				used := h.inflight.used
				h.inflight.mu.Unlock()
				if used == 0 {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("reservation leaked: used=%d", used)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

// The cache-fill path hands the buffer to the segment cache, which counts it
// in its own byte budget; the in-flight reservation must therefore be returned
// before the (possibly slow) response write instead of counting the same bytes
// twice for as long as the client takes to read them.
func TestServeStreamCacheFillReleasesBudgetBeforeWrite(t *testing.T) {
	const body = "hello segment"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()
	h := newStreamProxyForTest(t, upstream, time.Minute)

	var heldDuringWrite int64 = -1
	w := hookWriter{ResponseRecorder: httptest.NewRecorder()}
	w.onWrite = func() {
		h.inflight.mu.Lock()
		heldDuringWrite = h.inflight.used
		h.inflight.mu.Unlock()
	}
	h.serveStream(w, httptest.NewRequest("GET", "/proxy/stream?d="+url.QueryEscape(upstream.URL+"/s.ts"), nil))
	if w.Code != http.StatusOK || w.Body.String() != body {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if entries, _ := h.CacheStats(); entries != 1 {
		t.Fatalf("entries=%d, want the segment cached", entries)
	}
	if heldDuringWrite != 0 {
		t.Errorf("in-flight budget still held (%d bytes) while writing a segment the cache already owns", heldDuringWrite)
	}
	h.inflight.mu.Lock()
	defer h.inflight.mu.Unlock()
	if h.inflight.used != 0 {
		t.Errorf("reservation leaked: used=%d", h.inflight.used)
	}
}

// A failed cache-fill read must still return its reservation.
func TestServeStreamCacheFillReadErrorReleasesBudget(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "short") // body shorter than advertised
	}))
	defer upstream.Close()
	h := newStreamProxyForTest(t, upstream, time.Minute)

	w := httptest.NewRecorder()
	h.serveStream(w, httptest.NewRequest("GET", "/proxy/stream?d="+url.QueryEscape(upstream.URL+"/s.ts"), nil))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", w.Code)
	}
	h.inflight.mu.Lock()
	defer h.inflight.mu.Unlock()
	if h.inflight.used != 0 {
		t.Errorf("reservation leaked: used=%d", h.inflight.used)
	}
}

// deadlineRecorder is a ResponseWriter that supports per-write deadlines (like
// *http.response) and records every one armed, failing the nth write if asked.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
	writes    []int
	failAt    int // 1-based index of the write that fails; 0 = never
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	return nil
}

func (d *deadlineRecorder) Write(p []byte) (int, error) {
	d.writes = append(d.writes, len(p))
	if d.failAt > 0 && len(d.writes) == d.failAt {
		return 0, errors.New("write timeout")
	}
	return d.ResponseRecorder.Write(p)
}

// The idle deadline is re-armed before every chunk, so it limits progress, not
// total transfer time.
func TestWriteSegmentRearmsDeadlinePerChunk(t *testing.T) {
	p := bytes.Repeat([]byte("s"), 2*segmentWriteChunk+1000)
	d := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	idle := time.Minute

	start := time.Now()
	if err := writeSegment(d, p, idle); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(d.Body.Bytes(), p) {
		t.Fatal("body corrupted by chunked write")
	}
	if want := []int{segmentWriteChunk, segmentWriteChunk, 1000}; !slices.Equal(d.writes, want) {
		t.Fatalf("writes=%v, want %v", d.writes, want)
	}
	if len(d.deadlines) != len(d.writes) {
		t.Fatalf("%d deadlines armed for %d writes", len(d.deadlines), len(d.writes))
	}
	for i, dl := range d.deadlines {
		if dl.Before(start.Add(idle)) || dl.After(time.Now().Add(idle)) {
			t.Errorf("deadline %d = %v, want ~now+%v", i, dl, idle)
		}
		if i > 0 && dl.Before(d.deadlines[i-1]) {
			t.Errorf("deadline %d moved backwards", i)
		}
	}
}

func TestWriteSegmentStopsOnWriteError(t *testing.T) {
	p := bytes.Repeat([]byte("s"), 3*segmentWriteChunk)
	d := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder(), failAt: 2}
	if err := writeSegment(d, p, time.Minute); err == nil {
		t.Fatal("want the write error")
	}
	if len(d.writes) != 2 {
		t.Fatalf("kept writing after an error: writes=%v", d.writes)
	}
}

// Writers that cannot take deadlines (httptest recorders, exotic wrappers) are
// still written in full, and a non-positive idle arms nothing.
func TestWriteSegmentWithoutDeadlineSupport(t *testing.T) {
	p := bytes.Repeat([]byte("s"), segmentWriteChunk+5)
	rec := httptest.NewRecorder()
	if err := writeSegment(rec, p, time.Minute); err != nil || !bytes.Equal(rec.Body.Bytes(), p) {
		t.Fatalf("unsupported writer: err=%v ok=%v", err, bytes.Equal(rec.Body.Bytes(), p))
	}
	d := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	if err := writeSegment(d, p, 0); err != nil || len(d.deadlines) != 0 || !bytes.Equal(d.Body.Bytes(), p) {
		t.Fatalf("idle=0: err=%v deadlines=%d", err, len(d.deadlines))
	}
}
