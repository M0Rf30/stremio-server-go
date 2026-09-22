package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
)

// fakeTorrentReader is a minimal torrent.Reader stub used to drive pinnedReader
// without a real torrent/swarm. It never touches anacrolix internals, so this
// test is fully offline and deterministic.
//
// Read behaviour:
//   - call #1 returns a byte immediately (simulates data already buffered
//     locally — the fast-start case).
//   - call #2+ blocks until either the currently-installed context is
//     cancelled or dataReady is closed (simulating the swarm eventually
//     delivering the next chunk), mirroring waitAvailable's real select.
type fakeTorrentReader struct {
	mu    sync.Mutex
	ctx   context.Context
	calls int

	dataReady chan struct{}
}

func newFakeTorrentReader() *fakeTorrentReader {
	return &fakeTorrentReader{ctx: context.Background(), dataReady: make(chan struct{})}
}

func (f *fakeTorrentReader) SetContext(ctx context.Context) {
	f.mu.Lock()
	f.ctx = ctx
	f.mu.Unlock()
}

func (f *fakeTorrentReader) currentCtx() context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ctx
}

func (f *fakeTorrentReader) Read(b []byte) (int, error) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.mu.Unlock()

	if call == 1 {
		b[0] = 'x'
		return 1, nil
	}

	ctx := f.currentCtx()
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-f.dataReady:
		b[0] = 'y'
		return 1, nil
	}
}

func (f *fakeTorrentReader) ReadContext(ctx context.Context, b []byte) (int, error) {
	f.SetContext(ctx)
	return f.Read(b)
}

func (f *fakeTorrentReader) Seek(offset int64, _ int) (int64, error) { return offset, nil }
func (f *fakeTorrentReader) Close() error                            { return nil }
func (f *fakeTorrentReader) SetReadahead(int64)                      {}
func (f *fakeTorrentReader) SetReadaheadFunc(torrent.ReadaheadFunc)  {}
func (f *fakeTorrentReader) SetResponsive()                          {}

var _ torrent.Reader = (*fakeTorrentReader)(nil)

// TestPinnedReaderContextNotPoisonedAfterFirstRead is the bug-1 regression
// test: it reproduces the exact call sequence pinnedReader.Read used to run
// (deadline-context first read, cancel on return) and asserts the reader is
// left holding a live, long-lived context afterwards — not the cancelled
// deadline one — and that a second read which genuinely has to wait for the
// swarm blocks normally instead of failing instantly with context.Canceled.
func TestPinnedReaderContextNotPoisonedAfterFirstRead(t *testing.T) {
	fr := newFakeTorrentReader()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pr := &pinnedReader{
		tr:       fr,
		e:        &engine{},
		idx:      0,
		deadline: time.Now().Add(2 * time.Second),
		ctx:      ctx,
		cancel:   cancel,
	}

	buf := make([]byte, 1)
	n, err := pr.Read(buf)
	if err != nil || n != 1 {
		t.Fatalf("first Read: n=%d err=%v", n, err)
	}
	if !pr.started.Load() {
		t.Fatal("started flag not set after a successful first read")
	}

	// SetContext must have been called with the long-lived context, and that
	// context must not be cancelled.
	installed := fr.currentCtx()
	if installed != ctx {
		t.Fatalf("reader's installed context = %p; want the long-lived pinnedReader.ctx %p", installed, ctx)
	}
	if err := installed.Err(); err != nil {
		t.Fatalf("reader's context is already done after the first read (poisoned): %v", err)
	}

	// A second read that genuinely has to wait for the swarm must block, not
	// fail instantly with context.Canceled (the original bug: the deferred
	// cancel() from the first read's deadline context permanently poisoned
	// every subsequent Read).
	errCh := make(chan error, 1)
	go func() {
		_, err := pr.Read(buf)
		errCh <- err
	}()

	select {
	case err := <-errCh:
		t.Fatalf("second Read returned immediately instead of blocking for the swarm: err=%v", err)
	case <-time.After(50 * time.Millisecond):
		// Expected: still blocked, waiting genuinely.
	}

	close(fr.dataReady)

	select {
	case err := <-errCh:
		if errors.Is(err, context.Canceled) {
			t.Fatalf("second Read returned context.Canceled: reader context was poisoned by the first read's deadline cancel")
		}
		if err != nil {
			t.Fatalf("second Read: unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Read never returned after data became available")
	}
}
