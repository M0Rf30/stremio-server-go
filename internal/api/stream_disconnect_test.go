// Package api integration tests for the client-disconnect / reader-leak fix in
// handleStream (bug 2): a client that disconnects mid-stream while the
// underlying reader is blocked must have that reader closed promptly, and the
// engine's openReaders pin released, instead of leaking for process lifetime.
package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/types"
)

// blockingReadCloser serves a small amount of data immediately, then blocks
// forever on the next Read until Close is called — simulating a torrent
// reader stalled waiting on the swarm. blockedCh is closed the moment a Read
// call actually starts blocking, so the test can synchronize on it instead of
// sleeping.
type blockingReadCloser struct {
	data []byte
	pos  int

	blockedCh   chan struct{}
	blockedOnce sync.Once

	closeCh   chan struct{}
	closeOnce sync.Once
	onClose   func()

	closed atomic.Bool
}

func newBlockingReadCloser(data []byte, onClose func()) *blockingReadCloser {
	return &blockingReadCloser{
		data:      data,
		blockedCh: make(chan struct{}),
		closeCh:   make(chan struct{}),
		onClose:   onClose,
	}
}

func (b *blockingReadCloser) Read(p []byte) (int, error) {
	if b.pos < len(b.data) {
		n := copy(p, b.data[b.pos:])
		b.pos += n
		return n, nil
	}
	b.blockedOnce.Do(func() { close(b.blockedCh) })
	<-b.closeCh
	return 0, http.ErrBodyReadAfterClose
}

func (b *blockingReadCloser) Seek(_ int64, _ int) (int64, error) { return 0, nil }

func (b *blockingReadCloser) Close() error {
	b.closeOnce.Do(func() {
		b.closed.Store(true)
		close(b.closeCh)
		if b.onClose != nil {
			b.onClose()
		}
	})
	return nil
}

// disconnectFakeEngine is a minimal types.Engine backed by a single
// blockingReadCloser, with an openReaders counter mirroring the real engine's
// pin accounting (incremented by NewReader, decremented when the returned
// reader is Closed).
type disconnectFakeEngine struct {
	ih          string
	rc          *blockingReadCloser
	openReaders int32
}

func (e *disconnectFakeEngine) InfoHash() string              { return e.ih }
func (e *disconnectFakeEngine) Ready(_ context.Context) error { return nil }
func (e *disconnectFakeEngine) Files() []types.FileInfo {
	return []types.FileInfo{{Name: "video.mkv", Path: "video.mkv", Length: 1 << 20}}
}
func (e *disconnectFakeEngine) GuessFileIdx() int { return 0 }
func (e *disconnectFakeEngine) NewReader(_ int) (io.ReadSeekCloser, int64, error) {
	atomic.AddInt32(&e.openReaders, 1)
	return e.rc, 1 << 20, nil
}
func (e *disconnectFakeEngine) Stats(_ int) *types.Stats { return &types.Stats{} }

type disconnectFakeEM struct {
	e *disconnectFakeEngine
}

func (m *disconnectFakeEM) EnsureEngine(ih string, _ types.AddOptions) (types.Engine, error) {
	return m.e, nil
}
func (m *disconnectFakeEM) GetEngine(ih string) (types.Engine, bool) {
	if ih == m.e.ih {
		return m.e, true
	}
	return nil, false
}
func (m *disconnectFakeEM) RemoveEngine(_ string) error       { return nil }
func (m *disconnectFakeEM) RemoveAll()                        {}
func (m *disconnectFakeEM) ListEngines() []string             { return []string{m.e.ih} }
func (m *disconnectFakeEM) AllStats() map[string]*types.Stats { return nil }
func (m *disconnectFakeEM) Close() error                      { return nil }

var _ types.EngineManager = (*disconnectFakeEM)(nil)

// TestHandlerStream_ClientDisconnectClosesReaderAndReleasesOpenReaders is the
// bug-2 regression test. It drives handleStream (via the full HTTP handler)
// with a request whose context is cancelled mid-copy while the underlying
// reader is blocked, and asserts:
//  1. the handler returns promptly (the disconnect watcher unblocked it —
//     without the fix nothing ever wakes handleStream's blocked Read, so this
//     would hang until the test's own timeout fires);
//  2. the reader was actually Closed as a result;
//  3. openReaders is back to 0, i.e. the janitor's evict/evictIdle passes are
//     no longer permanently exempting this torrent.
func TestHandlerStream_ClientDisconnectClosesReaderAndReleasesOpenReaders(t *testing.T) {
	eng := &disconnectFakeEngine{ih: testIH}
	eng.rc = newBlockingReadCloser([]byte("abc"), func() {
		atomic.AddInt32(&eng.openReaders, -1)
	})

	h := New(&disconnectFakeEM{e: eng}, &fakeSS{}, &fakeProber{}, types.Config{
		HTTPPort: 11470,
		WebUI:    "https://web.stremio.com/",
	})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/"+testIH+"/0", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-eng.rc.blockedCh:
		// Reader is now genuinely stalled inside Read, matching a stalled
		// swarm mid-playback.
	case <-time.After(2 * time.Second):
		t.Fatal("reader never reached the blocking Read (test setup problem)")
	}

	if got := atomic.LoadInt32(&eng.openReaders); got != 1 {
		t.Fatalf("openReaders = %d before disconnect; want 1", got)
	}

	cancel() // simulate the client (tab close / seek abort) disconnecting

	select {
	case <-done:
		// Handler returned: the disconnect watcher unblocked the read.
	case <-time.After(2 * time.Second):
		t.Fatal("handleStream did not return after client disconnect; blocked read was never unblocked")
	}

	if !eng.rc.closed.Load() {
		t.Fatal("reader was not closed after client context cancellation")
	}
	if got := atomic.LoadInt32(&eng.openReaders); got != 0 {
		t.Fatalf("openReaders = %d after disconnect; want 0 (janitor would still skip this torrent)", got)
	}
}
