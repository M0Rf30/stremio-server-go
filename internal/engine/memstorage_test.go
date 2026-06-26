package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// syntheticInfo builds a single-file v1 metainfo.Info with numPieces full-length
// pieces. The piece hashes are zero (never verified by these white-box tests,
// which drive the storage backend directly without a real torrent/peers).
func syntheticInfo(pieceLen int64, numPieces int) *metainfo.Info {
	return &metainfo.Info{
		Name:        "memtest",
		PieceLength: pieceLen,
		Length:      pieceLen * int64(numPieces),
		Pieces:      make([]byte, metainfo.HashSize*numPieces),
	}
}

// fillPattern returns a deterministic pieceLen-byte pattern unique per index.
func fillPattern(idx int, pieceLen int64) []byte {
	return bytes.Repeat([]byte{byte(idx*7 + 1)}, int(pieceLen))
}

// readFull reads the whole piece via ReadAt. io.EOF at the boundary is not an
// error for our purposes (the io.ReaderAt contract allows it).
func readFull(p storage.PieceImpl, pieceLen int64) ([]byte, error) {
	buf := make([]byte, pieceLen)
	n, err := p.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:n], nil
}

func TestMemStorageWriteCompleteRead(t *testing.T) {
	const (
		pieceLen  = 256
		numPieces = 4
	)
	info := syntheticInfo(pieceLen, numPieces)
	s := newMemStorage(pieceLen * numPieces) // budget large enough to hold all
	t.Cleanup(func() { _ = s.Close() })

	ti, err := s.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatalf("OpenTorrent: %v", err)
	}
	t.Cleanup(func() {
		if ti.Close != nil {
			_ = ti.Close()
		}
	})

	if ti.Capacity == nil {
		t.Fatal("TorrentImpl.Capacity must be set so anacrolix bounds requests")
	}
	if cap, capped := (*ti.Capacity)(); !capped || cap != pieceLen*numPieces {
		t.Fatalf("Capacity() = (%d, %v), want (%d, true)", cap, capped, pieceLen*numPieces)
	}

	p := ti.Piece(info.Piece(0))

	// Before any write the piece is known-incomplete (Ok true so anacrolix trusts
	// it and requests the data), and reading it must error rather than return
	// (0, nil) — which the storage wrappers treat as a fatal protocol violation.
	if c := p.Completion(); !c.Ok || c.Complete {
		t.Fatalf("unwritten piece Completion = %+v, want {Ok:true, Complete:false}", c)
	}
	if _, err := p.ReadAt(make([]byte, pieceLen), 0); err == nil {
		t.Fatal("ReadAt on unwritten piece returned nil error; must signal not-resident")
	}

	want := fillPattern(0, pieceLen)
	// Write in two chunks to exercise offset handling.
	if n, err := p.WriteAt(want[:100], 0); n != 100 || err != nil {
		t.Fatalf("WriteAt(0) = (%d, %v), want (100, nil)", n, err)
	}
	if n, err := p.WriteAt(want[100:], 100); n != pieceLen-100 || err != nil {
		t.Fatalf("WriteAt(100) = (%d, %v), want (%d, nil)", n, err, pieceLen-100)
	}

	// Resident but not yet hash-verified.
	if c := p.Completion(); !c.Ok || c.Complete {
		t.Fatalf("written-but-unmarked piece Completion = %+v, want {Ok:true, Complete:false}", c)
	}
	// Data is readable before MarkComplete (anacrolix reads to verify the hash).
	got, err := readFull(p, pieceLen)
	if err != nil {
		t.Fatalf("ReadAt before MarkComplete: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("ReadAt before MarkComplete returned wrong bytes")
	}

	if err := p.MarkComplete(); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}
	if c := p.Completion(); !c.Ok || !c.Complete {
		t.Fatalf("post-MarkComplete Completion = %+v, want {Ok:true, Complete:true}", c)
	}
	got, err = readFull(p, pieceLen)
	if err != nil {
		t.Fatalf("ReadAt after MarkComplete: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("ReadAt after MarkComplete returned wrong bytes")
	}
}

func TestMemStorageEvictsOldestCompleteKeepsRecentlyRead(t *testing.T) {
	const (
		pieceLen  = 256
		numPieces = 4
		capacity  = 2 * pieceLen // room for exactly two resident pieces
	)
	info := syntheticInfo(pieceLen, numPieces)
	s := newMemStorage(capacity)
	t.Cleanup(func() { _ = s.Close() })

	ti, err := s.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatalf("OpenTorrent: %v", err)
	}
	t.Cleanup(func() {
		if ti.Close != nil {
			_ = ti.Close()
		}
	})

	writeComplete := func(idx int) storage.PieceImpl {
		t.Helper()
		p := ti.Piece(info.Piece(idx))
		if n, err := p.WriteAt(fillPattern(idx, pieceLen), 0); n != pieceLen || err != nil {
			t.Fatalf("piece %d WriteAt = (%d, %v)", idx, n, err)
		}
		if err := p.MarkComplete(); err != nil {
			t.Fatalf("piece %d MarkComplete: %v", idx, err)
		}
		return p
	}

	p0 := writeComplete(0)
	p1 := writeComplete(1) // budget now full: {p1(MRU), p0}

	// Touch p0 via ReadAt so it becomes most-recently-used: {p0(MRU), p1}.
	if _, err := readFull(p0, pieceLen); err != nil {
		t.Fatalf("read p0: %v", err)
	}

	// Writing a third piece must evict exactly one complete piece. p1 is now the
	// least-recently-used complete piece, so it is the victim; p0 (recently read)
	// is retained.
	p2 := writeComplete(2)

	// p1 evicted: reports not-complete AND ReadAt errors (never stale bytes).
	if c := p1.Completion(); c.Complete {
		t.Fatalf("evicted p1 Completion = %+v, want Complete:false", c)
	}
	if _, err := p1.ReadAt(make([]byte, pieceLen), 0); err == nil {
		t.Fatal("evicted p1 ReadAt returned nil error; must signal not-resident so anacrolix re-fetches")
	}

	// p0 retained: still complete and serves its original bytes.
	if c := p0.Completion(); !c.Ok || !c.Complete {
		t.Fatalf("retained p0 Completion = %+v, want {Ok:true, Complete:true}", c)
	}
	got, err := readFull(p0, pieceLen)
	if err != nil {
		t.Fatalf("read retained p0: %v", err)
	}
	if !bytes.Equal(got, fillPattern(0, pieceLen)) {
		t.Fatal("retained p0 returned wrong bytes after eviction")
	}

	// p2 is resident and complete.
	if c := p2.Completion(); !c.Ok || !c.Complete {
		t.Fatalf("new p2 Completion = %+v, want {Ok:true, Complete:true}", c)
	}

	// Hard budget respected: exactly two pieces (p0, p2) resident.
	ms := s.(*memStorage)
	ms.mu.Lock()
	used := ms.used
	ms.mu.Unlock()
	if used != capacity {
		t.Fatalf("resident bytes = %d, want %d (two pieces)", used, int64(capacity))
	}
}

// TestMemStorageConcurrentNeverWrongBytes hammers the backend from many
// goroutines under a tight budget (heavy eviction) and asserts the core
// correctness invariant: a successful ReadAt always returns the exact bytes
// written for that piece — never stale or wrong data. Run with -race, it also
// proves the backend is free of data races. Deterministic in outcome.
func TestMemStorageConcurrentNeverWrongBytes(t *testing.T) {
	const (
		pieceLen  = 128
		numPieces = 64
		capacity  = 8 * pieceLen // forces continual eviction
	)
	info := syntheticInfo(pieceLen, numPieces)
	s := newMemStorage(capacity)
	t.Cleanup(func() { _ = s.Close() })

	ti, err := s.OpenTorrent(context.Background(), info, metainfo.Hash{})
	if err != nil {
		t.Fatalf("OpenTorrent: %v", err)
	}
	t.Cleanup(func() {
		if ti.Close != nil {
			_ = ti.Close()
		}
	})

	var wg sync.WaitGroup
	for i := 0; i < numPieces; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			p := ti.Piece(info.Piece(idx))
			want := fillPattern(idx, pieceLen)
			if n, err := p.WriteAt(want, 0); n != pieceLen || err != nil {
				t.Errorf("piece %d WriteAt = (%d, %v)", idx, n, err)
				return
			}
			if err := p.MarkComplete(); err != nil {
				t.Errorf("piece %d MarkComplete: %v", idx, err)
				return
			}
			// Concurrently read back and query completion. If the read succeeds
			// the bytes MUST match; if the piece was evicted the read errors —
			// both are correct, returning the wrong bytes is not.
			for k := 0; k < 8; k++ {
				buf := make([]byte, pieceLen)
				n, rerr := p.ReadAt(buf, 0)
				if rerr == nil || errors.Is(rerr, io.EOF) {
					if !bytes.Equal(buf[:n], want[:n]) || n != pieceLen {
						t.Errorf("piece %d ReadAt returned wrong bytes", idx)
						return
					}
				}
				_ = p.Completion()
			}
		}(i)
	}
	wg.Wait()

	// Once quiescent, every piece is complete and the budget is enforced.
	ms := s.(*memStorage)
	ms.mu.Lock()
	used := ms.used
	ms.mu.Unlock()
	if used > capacity {
		t.Fatalf("resident bytes = %d exceed budget %d after settle", used, int64(capacity))
	}
}
