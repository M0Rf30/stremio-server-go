// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

// Package api — tests for progressive NZB serving: a Range request is
// answered as soon as the segments covering it are downloaded (the file is
// assembled in the background by parallel NNTP connections), a client
// disconnecting never kills the shared download, and a failed assembly is
// retried by the next request. A loopback fake NNTP server stands in for the
// Usenet provider; nothing leaves the machine.
package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/nzb"
)

// ---- fake NNTP server ------------------------------------------------------

type nntpFake struct {
	ln net.Listener
	wg sync.WaitGroup

	mu        sync.Mutex
	articles  map[string][]byte
	latency   time.Duration
	hook      func(id string) // runs before an article is sent; may block
	bodyCount map[string]int
	order     []string
	open      int
	live      map[net.Conn]struct{}
}

func newNNTPFake(t testing.TB, articles map[string][]byte) *nntpFake {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &nntpFake{ln: ln, articles: articles, bodyCount: map[string]int{}, live: map[net.Conn]struct{}{}}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				f.serve(c)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		f.mu.Lock()
		for c := range f.live {
			_ = c.Close()
		}
		f.mu.Unlock()
		f.wg.Wait()
	})
	return f
}

func (f *nntpFake) serve(c net.Conn) {
	defer c.Close()
	f.mu.Lock()
	f.open++
	f.live[c] = struct{}{}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.open--
		delete(f.live, c)
		f.mu.Unlock()
	}()

	tp := textproto.NewConn(c)
	_ = tp.PrintfLine("200 ready")
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		switch {
		case strings.HasPrefix(line, "QUIT"):
			_ = tp.PrintfLine("205 bye")
			return
		case strings.HasPrefix(line, "BODY "):
			id := strings.Trim(strings.TrimPrefix(line, "BODY "), "<>")
			f.mu.Lock()
			f.bodyCount[id]++
			f.order = append(f.order, id)
			art, ok := f.articles[id]
			hook, lat := f.hook, f.latency
			f.mu.Unlock()
			if hook != nil {
				hook(id)
			}
			if lat > 0 {
				time.Sleep(lat)
			}
			if !ok {
				_ = tp.PrintfLine("430 no such article")
				continue
			}
			_ = tp.PrintfLine("222 0 <%s>", id)
			dw := tp.DotWriter()
			_, _ = dw.Write(art)
			_ = dw.Close()
		default:
			_ = tp.PrintfLine("500 unknown command")
		}
	}
}

func (f *nntpFake) setHook(fn func(id string)) {
	f.mu.Lock()
	f.hook = fn
	f.mu.Unlock()
}

func (f *nntpFake) setLatency(d time.Duration) {
	f.mu.Lock()
	f.latency = d
	f.mu.Unlock()
}

func (f *nntpFake) setArticle(id string, art []byte) {
	f.mu.Lock()
	if art == nil {
		delete(f.articles, id)
	} else {
		f.articles[id] = art
	}
	f.mu.Unlock()
}

func (f *nntpFake) bodies(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodyCount[id]
}

func (f *nntpFake) fetchOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

func (f *nntpFake) openConns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.open
}

// ---- fixtures --------------------------------------------------------------

// nzbYencPart encodes data as one part of a multi-part yEnc article (begin is
// 1-based) — the shape real posters produce.
func nzbYencPart(data []byte, total, begin int64, part, parts int) []byte {
	var out bytes.Buffer
	fmt.Fprintf(&out, "=ybegin part=%d total=%d line=128 size=%d name=movie.mkv\r\n", part, parts, total)
	fmt.Fprintf(&out, "=ypart begin=%d end=%d\r\n", begin, begin+int64(len(data))-1)
	col := 0
	for _, b := range data {
		enc := b + 42
		switch enc {
		case 0x00, 0x0A, 0x0D, 0x3D:
			out.WriteByte('=')
			out.WriteByte(enc + 64)
			col += 2
		default:
			out.WriteByte(enc)
			col++
		}
		if col >= 128 {
			out.WriteString("\r\n")
			col = 0
		}
	}
	if col > 0 {
		out.WriteString("\r\n")
	}
	fmt.Fprintf(&out, "=yend size=%d part=%d pcrc32=%08x\r\n", len(data), part, crc32.ChecksumIEEE(data))
	return out.Bytes()
}

type nzbStreamFixture struct {
	content  []byte
	file     nzb.File
	articles map[string][]byte
	fake     *nntpFake
	sess     *nzbSession
	srv      *httptest.Server
	url      string // /nzb/stream/<key>/<name>
}

func (fx *nzbStreamFixture) segID(i int) string { return fx.file.Segments[i].MessageID }

// newNzbStreamFixture registers an NZB session backed by a fake NNTP server
// and serves the real handler over a loopback httptest.Server.
func newNzbStreamFixture(t *testing.T, name string, nseg, segSize, lastShort, connections int) *nzbStreamFixture {
	t.Helper()
	total := nseg*segSize - lastShort
	content := make([]byte, total)
	r := rand.New(rand.NewPCG(uint64(nseg), uint64(segSize))) //nolint:gosec // deterministic test data
	for i := range content {
		content[i] = byte(r.Uint32())
	}
	fx := &nzbStreamFixture{content: content, articles: map[string][]byte{}}
	fx.file = nzb.File{Name: name, Subject: name}
	for i := range nseg {
		lo := i * segSize
		hi := min(lo+segSize, total)
		art := nzbYencPart(content[lo:hi], int64(total), int64(lo+1), i+1, nseg)
		id := fmt.Sprintf("%s-%d@test", name, i+1)
		fx.articles[id] = art
		fx.file.Segments = append(fx.file.Segments, nzb.Segment{MessageID: id, Bytes: int64(len(art)), Number: i + 1})
		fx.file.Size += int64(len(art))
	}
	fx.fake = newNNTPFake(t, fx.articles)

	key := fmt.Sprintf("progressive-%s-%d", t.Name(), time.Now().UnixNano())
	fx.sess = &nzbSession{
		key: key,
		cfg: nzb.ServerConfig{
			Host:        "127.0.0.1",
			Port:        fx.fake.ln.Addr().(*net.TCPAddr).Port,
			Connections: connections,
		},
		files:      []nzb.File{fx.file},
		tmpDir:     t.TempDir(),
		created:    time.Now(),
		lastAccess: time.Now(),
		fileStates: map[string]*nzbFileState{},
	}
	nzbSessionsMu.Lock()
	nzbSessions[key] = fx.sess
	nzbSessionsMu.Unlock()
	t.Cleanup(func() {
		nzbSessionsMu.Lock()
		delete(nzbSessions, key)
		nzbSessionsMu.Unlock()
		fx.sess.discard()
	})

	fx.srv = httptest.NewServer(newHandler(t))
	t.Cleanup(fx.srv.Close)
	fx.url = fx.srv.URL + "/nzb/stream/" + key + "/" + name
	return fx
}

// assembly returns the file's current assembly (nil before the first request).
func (fx *nzbStreamFixture) assembly() *nzb.Assembly {
	fx.sess.mu.Lock()
	fs := fx.sess.fileStates[fx.file.Name]
	fx.sess.mu.Unlock()
	if fs == nil {
		return nil
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.asm
}

func (fx *nzbStreamFixture) get(ctx context.Context, t *testing.T, rangeHdr string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fx.url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rangeHdr != "" {
		req.Header.Set("Range", rangeHdr)
	}
	return http.DefaultClient.Do(req)
}

func tctx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// gateFrom blocks every article from segment index `from` on until the
// returned release func is called (idempotent; also run at test end).
func (fx *nzbStreamFixture) gateFrom(t *testing.T, from int) (release func()) {
	t.Helper()
	gate := make(chan struct{})
	gated := map[string]bool{}
	for i := from; i < len(fx.file.Segments); i++ {
		gated[fx.segID(i)] = true
	}
	fx.fake.setHook(func(id string) {
		if gated[id] {
			<-gate
		}
	})
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return release
}

// ---- tests -----------------------------------------------------------------

// The core guarantee: a Range request is answered while most of the file is
// still undownloaded. With the old implementation the response only started
// once every segment had been assembled, so the gated tail would hang it.
func TestNzbStream_RangeServedBeforeAssemblyCompletes(t *testing.T) {
	fx := newNzbStreamFixture(t, "movie.mkv", 12, 10_000, 1_234, 3)
	release := fx.gateFrom(t, 5) // segments 5..11 are withheld

	resp, err := fx.get(tctx(t, 15*time.Second), t, "bytes=100-2099")
	if err != nil {
		t.Fatalf("range request blocked on the unfinished tail: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if want := fmt.Sprintf("bytes 100-2099/%d", len(fx.content)); resp.Header.Get("Content-Range") != want {
		t.Errorf("Content-Range = %q, want %q", resp.Header.Get("Content-Range"), want)
	}
	if !bytes.Equal(body, fx.content[100:2100]) {
		t.Fatal("range body differs from the file content")
	}
	if a := fx.assembly(); a == nil || a.Complete() {
		t.Fatal("assembly must still be running while the tail is withheld")
	}

	// Same headers as before the change.
	for k, v := range map[string]string{
		"Accept-Ranges":            "bytes",
		"Content-Type":             "video/x-matroska",
		"transferMode.dlna.org":    "Streaming",
		"contentFeatures.dlna.org": "DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000",
	} {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
	lastMod := resp.Header.Get("Last-Modified")
	if lastMod == "" {
		t.Error("Last-Modified missing")
	}

	// HEAD needs only the size, also known before completion.
	hreq, _ := http.NewRequestWithContext(tctx(t, 10*time.Second), http.MethodHead, fx.url, nil)
	hresp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		t.Fatal(err)
	}
	hresp.Body.Close()
	if hresp.StatusCode != http.StatusOK || hresp.ContentLength != int64(len(fx.content)) {
		t.Errorf("HEAD: status %d, Content-Length %d; want 200, %d", hresp.StatusCode, hresp.ContentLength, len(fx.content))
	}

	release()
	<-fx.assembly().Done()
	if err := fx.assembly().Err(); err != nil {
		t.Fatal(err)
	}

	// After completion: full 200 body, identical Last-Modified (so a client's
	// If-Range keeps working across the transition), and a suffix range.
	full, err := fx.get(tctx(t, 10*time.Second), t, "")
	if err != nil {
		t.Fatal(err)
	}
	fullBody, _ := io.ReadAll(full.Body)
	full.Body.Close()
	if full.StatusCode != http.StatusOK || !bytes.Equal(fullBody, fx.content) {
		t.Fatalf("full GET: status %d, %d bytes (want 200, %d identical bytes)", full.StatusCode, len(fullBody), len(fx.content))
	}
	if got := full.Header.Get("Last-Modified"); got != lastMod {
		t.Errorf("Last-Modified changed once assembly finished: %q -> %q", lastMod, got)
	}
	tail, err := fx.get(tctx(t, 10*time.Second), t, "bytes=-500")
	if err != nil {
		t.Fatal(err)
	}
	tailBody, _ := io.ReadAll(tail.Body)
	tail.Body.Close()
	if tail.StatusCode != http.StatusPartialContent || !bytes.Equal(tailBody, fx.content[len(fx.content)-500:]) {
		t.Fatalf("suffix range: status %d, body mismatch", tail.StatusCode)
	}
	// An unsatisfiable range is still 416.
	bad, err := fx.get(tctx(t, 10*time.Second), t, fmt.Sprintf("bytes=%d-", len(fx.content)+10))
	if err != nil {
		t.Fatal(err)
	}
	bad.Body.Close()
	if bad.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("out-of-range status = %d, want 416", bad.StatusCode)
	}
}

// A Range at the end of the file (MP4 moov / MKV cues) must steer the
// downloader there instead of waiting for the sweep to reach it.
func TestNzbStream_SeekToTailPrioritisesThoseSegments(t *testing.T) {
	const nseg = 40
	fx := newNzbStreamFixture(t, "movie.mp4", nseg, 6_000, 0, 1)
	fx.fake.setLatency(8 * time.Millisecond)

	resp, err := fx.get(tctx(t, 15*time.Second), t, "bytes=-300")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, fx.content[len(fx.content)-300:]) {
		t.Fatalf("tail range: status %d, body mismatch", resp.StatusCode)
	}
	order := fx.fake.fetchOrder()
	pos := -1
	for i, id := range order {
		if id == fx.segID(nseg-1) {
			pos = i
		}
	}
	if pos < 0 || pos > 4 {
		t.Fatalf("last segment fetched at position %d (%d fetched so far); want within the first few", pos, len(order))
	}
	if a := fx.assembly(); a.Complete() {
		t.Fatal("tail request must not have waited for the whole file")
	}
}

// A client that disconnects mid-download must not stop the shared assembly or
// force the next client to re-download anything.
func TestNzbStream_ClientDisconnectKeepsAssemblyRunning(t *testing.T) {
	fx := newNzbStreamFixture(t, "movie.mkv", 10, 8_000, 0, 2)
	release := fx.gateFrom(t, 4)

	ctx, cancel := context.WithCancel(context.Background())
	resp, err := fx.get(ctx, t, "")
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, 1_000)
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatalf("first bytes: %v", err)
	}
	if !bytes.Equal(first, fx.content[:1_000]) {
		t.Fatal("first bytes differ")
	}
	cancel() // client goes away while the gated tail is outstanding
	resp.Body.Close()

	// Give the server a moment to observe the disconnect, then prove the
	// assembly is untouched.
	time.Sleep(100 * time.Millisecond)
	asm := fx.assembly()
	if err := asm.Err(); err != nil {
		t.Fatalf("client disconnect failed the shared assembly: %v", err)
	}

	release()
	select {
	case <-asm.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("assembly did not finish after the client left")
	}
	if err := asm.Err(); err != nil || !asm.Complete() {
		t.Fatalf("assembly: err=%v complete=%v", err, asm.Complete())
	}

	next, err := fx.get(tctx(t, 10*time.Second), t, "")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(next.Body)
	next.Body.Close()
	if !bytes.Equal(body, fx.content) {
		t.Fatal("second client did not get the complete file")
	}
	for i := range fx.file.Segments {
		if n := fx.fake.bodies(fx.segID(i)); n != 1 {
			t.Errorf("segment %d fetched %d times, want exactly once (single-flight)", i, n)
		}
	}
}

func TestNzbStream_ConcurrentRequestsShareOneAssembly(t *testing.T) {
	fx := newNzbStreamFixture(t, "movie.mkv", 20, 5_000, 777, 4)
	fx.fake.setLatency(2 * time.Millisecond)

	const clients = 8
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	for c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hdr := ""
			if c%2 == 1 { // mix full GETs and ranges
				hdr = fmt.Sprintf("bytes=%d-%d", c*4_000, c*4_000+9_999)
			}
			resp, err := fx.get(tctx(t, 20*time.Second), t, hdr)
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				errs <- fmt.Errorf("client %d: %w", c, err)
				return
			}
			want := fx.content
			if hdr != "" {
				want = fx.content[c*4_000 : c*4_000+10_000]
			}
			if !bytes.Equal(body, want) {
				errs <- fmt.Errorf("client %d: body mismatch (%d bytes, want %d)", c, len(body), len(want))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for i := range fx.file.Segments {
		if n := fx.fake.bodies(fx.segID(i)); n != 1 {
			t.Errorf("segment %d fetched %d times for %d concurrent clients, want once", i, n, clients)
		}
	}
}

// A first-segment failure is reported as 502 (as before) and the next request
// restarts the assembly from scratch.
func TestNzbStream_FailedAssemblyReports502ThenRetries(t *testing.T) {
	fx := newNzbStreamFixture(t, "movie.mkv", 6, 4_000, 0, 2)
	saved := fx.articles[fx.segID(0)]
	fx.fake.setArticle(fx.segID(0), nil) // 430 No Such Article

	resp, err := fx.get(tctx(t, 15*time.Second), t, "")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "nzb assemble:") {
		t.Errorf("error body %q lacks the \"nzb assemble:\" prefix", body)
	}
	failed := fx.assembly()
	<-failed.Done()

	fx.fake.setArticle(fx.segID(0), saved) // the article is back
	ok, err := fx.get(tctx(t, 15*time.Second), t, "")
	if err != nil {
		t.Fatal(err)
	}
	okBody, _ := io.ReadAll(ok.Body)
	ok.Body.Close()
	if ok.StatusCode != http.StatusOK || !bytes.Equal(okBody, fx.content) {
		t.Fatalf("retry: status %d, %d bytes; want 200 and the full file", ok.StatusCode, len(okBody))
	}
	if fx.assembly() == failed {
		t.Fatal("the failed assembly was not replaced")
	}
}

// A segment missing mid-file: bytes before it are served correctly, then the
// response is cut short (never padded with garbage).
func TestNzbStream_MidFileFailureTruncatesResponse(t *testing.T) {
	fx := newNzbStreamFixture(t, "movie.mkv", 8, 6_000, 0, 1)
	fx.fake.setArticle(fx.segID(5), nil)

	resp, err := fx.get(tctx(t, 15*time.Second), t, "")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (failure happens after headers)", resp.StatusCode)
	}
	if readErr == nil {
		t.Fatal("a response cut short must surface as a read error")
	}
	if len(body) > 5*6_000 || !bytes.Equal(body, fx.content[:len(body)]) {
		t.Fatalf("served %d bytes; must be a correct prefix no longer than the 5 good segments", len(body))
	}
	// Ranges inside the finished part still work on a fresh assembly... until
	// the same article is hit again; the early range itself must be exact.
	early, err := fx.get(tctx(t, 15*time.Second), t, "bytes=0-999")
	if err != nil {
		t.Fatal(err)
	}
	earlyBody, _ := io.ReadAll(early.Body)
	early.Body.Close()
	if early.StatusCode != http.StatusPartialContent || !bytes.Equal(earlyBody, fx.content[:1000]) {
		t.Fatalf("early range after failure: status %d, mismatch", early.StatusCode)
	}
}

// Eviction (and key replacement) must stop the background download, release
// the NNTP connections and delete the temp dir.
func TestNzbSession_DiscardStopsAssemblyAndRemovesTempDir(t *testing.T) {
	fx := newNzbStreamFixture(t, "movie.mkv", 10, 8_000, 0, 3)
	release := fx.gateFrom(t, 3)

	resp, err := fx.get(tctx(t, 15*time.Second), t, "bytes=0-99")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	asm := fx.assembly()
	if asm == nil || asm.Complete() {
		t.Fatal("expected a running assembly")
	}

	done := make(chan struct{})
	go func() { fx.sess.discard(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("discard hung")
	}
	select {
	case <-asm.Done():
	default:
		t.Fatal("discard returned before the workers exited")
	}
	if asm.Err() == nil {
		t.Error("discarded assembly must report cancellation")
	}
	if _, err := os.Stat(fx.sess.tmpDir); !os.IsNotExist(err) {
		t.Errorf("temp dir still present after discard: %v", err)
	}
	// The client side hung up; let the fake's handlers (blocked on the gate)
	// notice, so its open-connection count reflects the hang-up.
	release()
	deadline := time.Now().Add(5 * time.Second)
	for fx.fake.openConns() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := fx.fake.openConns(); n != 0 {
		t.Errorf("%d NNTP connections left open after discard", n)
	}
}

func TestNzbEvictIdle_CancelsRunningAssembly(t *testing.T) {
	fx := newNzbStreamFixture(t, "movie.mkv", 6, 5_000, 0, 2)
	fx.gateFrom(t, 2)
	resp, err := fx.get(tctx(t, 15*time.Second), t, "bytes=0-9")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	asm := fx.assembly()

	nzbSessionsMu.Lock()
	fx.sess.lastAccess = time.Now().Add(-2 * time.Hour)
	nzbSessionsMu.Unlock()
	nzbEvictIdle()

	nzbSessionsMu.Lock()
	_, still := nzbSessions[fx.sess.key]
	nzbSessionsMu.Unlock()
	if still {
		t.Fatal("idle session should have been evicted")
	}
	select {
	case <-asm.Done():
	default:
		t.Fatal("evicting the session left its assembly running")
	}
}

// Seek is pure arithmetic on the known size (http.ServeContent relies on
// Seek(0, SeekEnd) to learn the length before any byte exists on disk).
func TestNzbProgressiveFile_Seek(t *testing.T) {
	p := &nzbProgressiveFile{size: 1000}
	cases := []struct {
		off     int64
		whence  int
		want    int64
		wantErr bool
	}{
		{0, io.SeekEnd, 1000, false},
		{-100, io.SeekEnd, 900, false},
		{50, io.SeekStart, 50, false},
		{25, io.SeekCurrent, 75, false},
		{2000, io.SeekStart, 2000, false}, // past EOF is allowed; Read returns io.EOF
		{-1, io.SeekStart, 0, true},
		{-5000, io.SeekEnd, 0, true},
		{0, 99, 0, true},
	}
	for _, c := range cases {
		got, err := p.Seek(c.off, c.whence)
		if (err != nil) != c.wantErr || (err == nil && got != c.want) {
			t.Errorf("Seek(%d, %d) = %d, %v; want %d (err=%v)", c.off, c.whence, got, err, c.want, c.wantErr)
		}
	}
	if _, err := p.Seek(1000, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if n, err := p.Read(make([]byte, 8)); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("Read at EOF = %d, %v; want 0, io.EOF", n, err)
	}
}
