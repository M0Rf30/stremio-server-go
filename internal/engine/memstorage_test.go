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
	ms := s
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
	ms := s
	ms.mu.Lock()
	used := ms.used
	ms.mu.Unlock()
	if used > capacity {
		t.Fatalf("resident bytes = %d exceed budget %d after settle", used, int64(capacity))
	}
}

// TestMemStorageWriteAtBoundsCheck covers the two error returns in WriteAt:
// a negative offset and an offset at-or-beyond the piece length.
func TestMemStorageWriteAtBoundsCheck(t *testing.T) {
	const pieceLen = 64
	info := syntheticInfo(pieceLen, 1)
	s := newMemStorage(pieceLen)
	t.Cleanup(func() { _ = s.Close() })

	ti, err := s.OpenTorrent(t.Context(), info, metainfo.Hash{})
	if err != nil {
		t.Fatalf("OpenTorrent: %v", err)
	}
	t.Cleanup(func() {
		if ti.Close != nil {
			_ = ti.Close()
		}
	})
	p := ti.Piece(info.Piece(0))

	cases := []struct {
		name string
		off  int64
	}{
		{"negative", -1},
		{"at length", pieceLen},
		{"beyond length", pieceLen + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, err := p.WriteAt([]byte("x"), tc.off)
			if err == nil {
				t.Errorf("WriteAt(off=%d): expected error, got nil", tc.off)
			}
			if n != 0 {
				t.Errorf("WriteAt(off=%d): expected 0 bytes written, got %d", tc.off, n)
			}
		})
	}
}

// TestMemStorageReadAtBoundsCheck covers the negative-offset and out-of-range
// paths in ReadAt, plus the partial-fill (short read returning io.EOF).
func TestMemStorageReadAtBoundsCheck(t *testing.T) {
	const pieceLen = 64
	info := syntheticInfo(pieceLen, 1)
	s := newMemStorage(pieceLen)
	t.Cleanup(func() { _ = s.Close() })

	ti, err := s.OpenTorrent(t.Context(), info, metainfo.Hash{})
	if err != nil {
		t.Fatalf("OpenTorrent: %v", err)
	}
	t.Cleanup(func() {
		if ti.Close != nil {
			_ = ti.Close()
		}
	})
	p := ti.Piece(info.Piece(0))

	// Write data to make the piece resident.
	data := fillPattern(0, pieceLen)
	if _, err := p.WriteAt(data, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	t.Run("negative offset", func(t *testing.T) {
		n, err := p.ReadAt(make([]byte, 1), -1)
		if err == nil {
			t.Error("ReadAt(-1): expected error, got nil")
		}
		if n != 0 {
			t.Errorf("ReadAt(-1): expected 0 bytes, got %d", n)
		}
	})

	t.Run("at length", func(t *testing.T) {
		n, err := p.ReadAt(make([]byte, 1), pieceLen)
		if !errors.Is(err, io.EOF) {
			t.Errorf("ReadAt(at length): expected io.EOF, got %v", err)
		}
		if n != 0 {
			t.Errorf("ReadAt(at length): expected 0 bytes, got %d", n)
		}
	})

	t.Run("beyond length", func(t *testing.T) {
		n, err := p.ReadAt(make([]byte, 1), pieceLen+10)
		if !errors.Is(err, io.EOF) {
			t.Errorf("ReadAt(beyond length): expected io.EOF, got %v", err)
		}
		if n != 0 {
			t.Errorf("ReadAt(beyond length): want 0 bytes, got %d", n)
		}
	})

	t.Run("partial fill at end", func(t *testing.T) {
		const start = 10
		buf := make([]byte, pieceLen) // bigger than remaining (pieceLen-start)
		n, err := p.ReadAt(buf, start)
		if !errors.Is(err, io.EOF) {
			t.Errorf("ReadAt partial: expected io.EOF, got %v", err)
		}
		want := pieceLen - start
		if n != want {
			t.Errorf("ReadAt partial: got %d bytes, want %d", n, want)
		}
		if !bytes.Equal(buf[:n], data[start:]) {
			t.Error("ReadAt partial: wrong bytes returned")
		}
	})
}

// TestMemStorageMarkNotComplete verifies that MarkNotComplete flips the piece
// back to incomplete while keeping the resident buffer intact (anacrolix can
// then overwrite and re-verify without a reallocation).
func TestMemStorageMarkNotComplete(t *testing.T) {
	const pieceLen = 64
	info := syntheticInfo(pieceLen, 1)
	s := newMemStorage(pieceLen)
	t.Cleanup(func() { _ = s.Close() })

	ti, err := s.OpenTorrent(t.Context(), info, metainfo.Hash{})
	if err != nil {
		t.Fatalf("OpenTorrent: %v", err)
	}
	t.Cleanup(func() {
		if ti.Close != nil {
			_ = ti.Close()
		}
	})
	p := ti.Piece(info.Piece(0))
	want := fillPattern(0, pieceLen)

	if _, err := p.WriteAt(want, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := p.MarkComplete(); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}
	if c := p.Completion(); !c.Complete {
		t.Fatal("post-MarkComplete: Complete must be true")
	}

	// MarkNotComplete flips Complete back to false.
	if err := p.MarkNotComplete(); err != nil {
		t.Fatalf("MarkNotComplete: %v", err)
	}
	c := p.Completion()
	if c.Complete {
		t.Error("post-MarkNotComplete: Complete must be false")
	}
	if !c.Ok {
		t.Error("post-MarkNotComplete: Ok must remain true")
	}
	// Data must still be resident so anacrolix can overwrite and re-verify.
	got, err := readFull(p, pieceLen)
	if err != nil {
		t.Fatalf("ReadAt after MarkNotComplete: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("ReadAt after MarkNotComplete: buffer must retain original bytes")
	}
}

// TestMemStorageMarkCompleteBeforeWrite covers the nil-data guard in MarkComplete:
// calling it on an unwritten piece must return errPieceEvicted, not panic.
func TestMemStorageMarkCompleteBeforeWrite(t *testing.T) {
	const pieceLen = 64
	info := syntheticInfo(pieceLen, 1)
	s := newMemStorage(pieceLen)
	t.Cleanup(func() { _ = s.Close() })

	ti, err := s.OpenTorrent(t.Context(), info, metainfo.Hash{})
	if err != nil {
		t.Fatalf("OpenTorrent: %v", err)
	}
	t.Cleanup(func() {
		if ti.Close != nil {
			_ = ti.Close()
		}
	})
	p := ti.Piece(info.Piece(0))

	if err := p.MarkComplete(); err == nil {
		t.Error("MarkComplete on unwritten piece: expected error (errPieceEvicted), got nil")
	}
}

// TestMemStorageTorrentCloseFreesMemory verifies that closing a torrent via the
// TorrentImpl.Close callback drops all its resident pieces and decrements used
// bytes back to zero.
func TestMemStorageTorrentCloseFreesMemory(t *testing.T) {
	const (
		pieceLen  = 64
		numPieces = 2
	)
	info := syntheticInfo(pieceLen, numPieces)
	s := newMemStorage(pieceLen * numPieces)
	t.Cleanup(func() { _ = s.Close() })

	ti, err := s.OpenTorrent(t.Context(), info, metainfo.Hash{})
	if err != nil {
		t.Fatalf("OpenTorrent: %v", err)
	}

	// Write and complete both pieces so they are resident.
	for i := 0; i < numPieces; i++ {
		p := ti.Piece(info.Piece(i))
		if _, err := p.WriteAt(fillPattern(i, pieceLen), 0); err != nil {
			t.Fatalf("piece %d WriteAt: %v", i, err)
		}
		if err := p.MarkComplete(); err != nil {
			t.Fatalf("piece %d MarkComplete: %v", i, err)
		}
	}

	ms := s
	ms.mu.Lock()
	before := ms.used
	ms.mu.Unlock()
	if before != pieceLen*numPieces {
		t.Fatalf("before Close: used = %d, want %d", before, int64(pieceLen*numPieces))
	}

	if ti.Close == nil {
		t.Fatal("TorrentImpl.Close must be set")
	}
	if err := ti.Close(); err != nil {
		t.Fatalf("torrent Close: %v", err)
	}

	ms.mu.Lock()
	after := ms.used
	ms.mu.Unlock()
	if after != 0 {
		t.Errorf("after torrent Close: used = %d, want 0", after)
	}
}

// TestMemStorageEvictLockedNoVictim covers the "no evictable victim" early
// return in evictLocked. When all resident pieces are incomplete (in-flight),
// eviction cannot free space and the implementation tolerates a transient
// overage rather than dropping live data. The test writes two pieces into a
// budget sized for one, without completing either, and asserts both remain
// resident (used = 2×pieceLen > capacity).
func TestMemStorageEvictLockedNoVictim(t *testing.T) {
	const pieceLen = 64
	info := syntheticInfo(pieceLen, 2)
	s := newMemStorage(pieceLen) // only room for one piece
	t.Cleanup(func() { _ = s.Close() })

	ti, err := s.OpenTorrent(t.Context(), info, metainfo.Hash{})
	if err != nil {
		t.Fatalf("OpenTorrent: %v", err)
	}
	t.Cleanup(func() {
		if ti.Close != nil {
			_ = ti.Close()
		}
	})

	p0 := ti.Piece(info.Piece(0))
	p1 := ti.Piece(info.Piece(1))

	// Write piece 0 without completing it — it is in-flight (incomplete).
	if _, err := p0.WriteAt(fillPattern(0, pieceLen), 0); err != nil {
		t.Fatalf("piece 0 WriteAt: %v", err)
	}

	// Write piece 1: evictLocked(pieceLen, p1) searches the LRU for a complete
	// victim, finds none (p0 is incomplete), and returns without evicting.
	// The implementation tolerates this in-flight overage.
	if _, err := p1.WriteAt(fillPattern(1, pieceLen), 0); err != nil {
		t.Fatalf("piece 1 WriteAt with no evictable victim: %v", err)
	}

	// Both pieces are resident: used must equal 2×pieceLen, exceeding capacity.
	ms := s
	ms.mu.Lock()
	used := ms.used
	ms.mu.Unlock()
	if used != 2*pieceLen {
		t.Errorf("used = %d; want %d (in-flight overage tolerated)", used, int64(2*pieceLen))
	}

	// Verify both pieces still return their correct bytes.
	for idx, p := range []storage.PieceImpl{p0, p1} {
		got, err := readFull(p, pieceLen)
		if err != nil {
			t.Fatalf("piece %d readFull: %v", idx, err)
		}
		if !bytes.Equal(got, fillPattern(idx, pieceLen)) {
			t.Errorf("piece %d: wrong bytes after no-victim eviction", idx)
		}
	}
}

// refetchCall records one invocation of the memStorage refetch hook.
type refetchCall struct {
	ih    metainfo.Hash
	piece int
}

// TestMemStorageEvictedReadTriggersRefetch verifies the stack-overflow guard:
// reading a piece whose bytes were evicted invokes the refetch hook with the
// torrent's infohash and the piece index (so the engine can force a real
// re-download), and still returns errPieceEvicted so anacrolix re-fetches rather
// than serving stale bytes.
func TestMemStorageEvictedReadTriggersRefetch(t *testing.T) {
	const (
		pieceLen  = 256
		numPieces = 3
	)
	info := syntheticInfo(pieceLen, numPieces)
	s := newMemStorage(pieceLen) // budget = exactly one piece → forces eviction
	s.refetchBackoff = 0         // no artificial delay in tests
	t.Cleanup(func() { _ = s.Close() })

	var (
		mu    sync.Mutex
		calls []refetchCall
	)
	s.refetch = func(ih metainfo.Hash, piece int) {
		mu.Lock()
		calls = append(calls, refetchCall{ih: ih, piece: piece})
		mu.Unlock()
	}

	ih := metainfo.NewHashFromHex("0123456789abcdef0123456789abcdef01234567")
	ti, err := s.OpenTorrent(context.Background(), info, ih)
	if err != nil {
		t.Fatalf("OpenTorrent: %v", err)
	}
	t.Cleanup(func() {
		if ti.Close != nil {
			_ = ti.Close()
		}
	})

	// Complete piece 0, then complete piece 1; the one-piece budget evicts piece 0.
	p0 := ti.Piece(info.Piece(0))
	if _, err := p0.WriteAt(fillPattern(0, pieceLen), 0); err != nil {
		t.Fatalf("WriteAt p0: %v", err)
	}
	if err := p0.MarkComplete(); err != nil {
		t.Fatalf("MarkComplete p0: %v", err)
	}
	p1 := ti.Piece(info.Piece(1))
	if _, err := p1.WriteAt(fillPattern(1, pieceLen), 0); err != nil {
		t.Fatalf("WriteAt p1: %v", err)
	}
	if err := p1.MarkComplete(); err != nil {
		t.Fatalf("MarkComplete p1: %v", err)
	}

	// Piece 0's bytes are gone: it now reads as not-complete and ReadAt errors.
	if c := p0.Completion(); !c.Ok || c.Complete {
		t.Fatalf("evicted p0 Completion = %+v, want {Ok:true, Complete:false}", c)
	}
	if _, err := p0.ReadAt(make([]byte, pieceLen), 0); !errors.Is(err, errPieceEvicted) {
		t.Fatalf("ReadAt(evicted) err = %v, want errPieceEvicted", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("refetch invoked %d times, want 1: %+v", len(calls), calls)
	}
	if calls[0].ih != ih || calls[0].piece != 0 {
		t.Fatalf("refetch(%x, %d), want (%x, 0)", calls[0].ih, calls[0].piece, ih)
	}
}

// TestMemStoragePastEndReadNoRefetch verifies that a RESIDENT, complete piece
// read at/after its end returns the standard (0, io.EOF) io.ReaderAt result
// WITHOUT invoking the refetch+backoff guard. io.EOF is terminal, so anacrolix
// does not spin on it; firing refetch (and the 100 ms backoff) on this branch
// would add a spurious VerifyData call and latency to every legitimate
// end-of-piece read. The refetch guard is reserved for genuinely evicted
// pieces (covered by TestMemStorageEvictedReadTriggersRefetch).
func TestMemStoragePastEndReadNoRefetch(t *testing.T) {
	const pieceLen = 256
	info := syntheticInfo(pieceLen, 2)
	s := newMemStorage(pieceLen * 2)
	s.refetchBackoff = 0
	t.Cleanup(func() { _ = s.Close() })

	var (
		mu    sync.Mutex
		calls []refetchCall
	)
	s.refetch = func(ih metainfo.Hash, piece int) {
		mu.Lock()
		calls = append(calls, refetchCall{ih: ih, piece: piece})
		mu.Unlock()
	}

	ih := metainfo.NewHashFromHex("89abcdef0123456789abcdef0123456789abcdef")
	ti, err := s.OpenTorrent(context.Background(), info, ih)
	if err != nil {
		t.Fatalf("OpenTorrent: %v", err)
	}
	t.Cleanup(func() {
		if ti.Close != nil {
			_ = ti.Close()
		}
	})

	p := ti.Piece(info.Piece(1))
	if _, err := p.WriteAt(fillPattern(1, pieceLen), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := p.MarkComplete(); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}

	// In-bounds read still succeeds without any refetch.
	if n, err := p.ReadAt(make([]byte, 16), 0); n != 16 || err != nil {
		t.Fatalf("in-bounds ReadAt = (%d, %v), want (16, nil)", n, err)
	}
	// Read at the piece end: standard zero-byte io.EOF, and NO refetch fires.
	if n, err := p.ReadAt(make([]byte, 16), pieceLen); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("past-end ReadAt = (%d, %v), want (0, io.EOF)", n, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 0 {
		t.Fatalf("resident past-end read must not refetch; got calls = %+v", calls)
	}
}
