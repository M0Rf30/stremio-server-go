// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/nzb"
	"github.com/M0Rf30/stremio-server-go/internal/types"
)

// BenchmarkNzbStreamFirstByte guards NZB time-to-first-byte: GET
// /nzb/stream with Range: bytes=0-1023 against a fake NNTP server that adds a
// fixed latency to every article. Before progressive serving the response
// could only start after every segment had been downloaded one by one
// (ttfb ≈ total ≈ segments × latency); now it starts as soon as the first
// segment is on disk and the rest is fetched over parallel connections in the
// background. Reported metrics are the mean time to the first byte and until
// the whole file is assembled.
func BenchmarkNzbStreamFirstByte(b *testing.B) {
	const (
		nseg    = 40
		segSize = 64 * 1024
		latency = 5 * time.Millisecond
	)
	for _, conns := range []int{1, 4, 8} {
		b.Run(fmt.Sprintf("conns-%d", conns), func(b *testing.B) {
			fx := newNzbBenchFixture(b, nseg, segSize, latency)
			h := New(newFakeEM(), &fakeSS{}, &fakeProber{}, types.Config{
				HTTPPort:           11470,
				WebUI:              "https://web.stremio.com/",
				CreateMetadataWait: 90 * time.Second,
			})
			var ttfb, total time.Duration
			var iters int
			for b.Loop() {
				sess := fx.newSession(b, conns)
				req := httptest.NewRequest(http.MethodGet, "/nzb/stream/"+sess.key+"/movie.mkv", nil)
				req.Header.Set("Range", "bytes=0-1023")
				rec := httptest.NewRecorder()
				t0 := time.Now()
				h.ServeHTTP(rec, req)
				ttfb += time.Since(t0)
				if rec.Code != http.StatusPartialContent {
					b.Fatalf("status = %d; body %s", rec.Code, rec.Body.String())
				}
				fx.waitAssembled(b, sess)
				total += time.Since(t0)
				iters++
				fx.drop(sess)
			}
			b.ReportMetric(float64(ttfb.Microseconds())/1000/float64(iters), "ttfb-ms")
			b.ReportMetric(float64(total.Microseconds())/1000/float64(iters), "total-ms")
		})
	}
}

type nzbBenchFixture struct {
	file nzb.File
	port int
	n    int
}

func newNzbBenchFixture(b *testing.B, nseg, segSize int, latency time.Duration) *nzbBenchFixture {
	b.Helper()
	total := nseg * segSize
	content := make([]byte, total)
	for i := range content {
		content[i] = byte(i*131 + i>>8)
	}
	articles := map[string][]byte{}
	file := nzb.File{Name: "movie.mkv", Subject: "movie.mkv"}
	for i := range nseg {
		lo := i * segSize
		art := nzbYencPart(content[lo:lo+segSize], int64(total), int64(lo+1), i+1, nseg)
		id := fmt.Sprintf("bench-%d@test", i+1)
		articles[id] = art
		file.Segments = append(file.Segments, nzb.Segment{MessageID: id, Bytes: int64(len(art)), Number: i + 1})
		file.Size += int64(len(art))
	}
	f := newNNTPFake(b, articles)
	f.setLatency(latency)
	return &nzbBenchFixture{file: file, port: f.ln.Addr().(*net.TCPAddr).Port}
}

func (fx *nzbBenchFixture) newSession(b *testing.B, conns int) *nzbSession {
	b.Helper()
	fx.n++
	sess := &nzbSession{
		key:        fmt.Sprintf("bench-%d-%d", time.Now().UnixNano(), fx.n),
		cfg:        nzb.ServerConfig{Host: "127.0.0.1", Port: fx.port, Connections: conns},
		files:      []nzb.File{fx.file},
		tmpDir:     b.TempDir(),
		created:    time.Now(),
		lastAccess: time.Now(),
		fileStates: map[string]*nzbFileState{},
	}
	nzbSessionsMu.Lock()
	nzbSessions[sess.key] = sess
	nzbSessionsMu.Unlock()
	return sess
}

// waitAssembled blocks until the session's background assembly has finished.
func (fx *nzbBenchFixture) waitAssembled(b *testing.B, sess *nzbSession) {
	b.Helper()
	sess.mu.Lock()
	fs := sess.fileStates["movie.mkv"]
	sess.mu.Unlock()
	fs.mu.Lock()
	asm := fs.asm
	fs.mu.Unlock()
	select {
	case <-asm.Done():
	case <-time.After(30 * time.Second):
		b.Fatal("assembly did not finish")
	}
	if err := asm.Err(); err != nil {
		b.Fatal(err)
	}
}

func (fx *nzbBenchFixture) drop(sess *nzbSession) {
	nzbSessionsMu.Lock()
	delete(nzbSessions, sess.key)
	nzbSessionsMu.Unlock()
	sess.discard()
}
