package engine

// In-RAM, bounded torrent storage backend (opt-in).
//
// This file implements a storage.ClientImplCloser that keeps piece data in
// memory instead of on disk, bounded by a single byte budget shared across all
// torrents. It lets the server stream without ever writing piece data to disk
// (mobile / Termux / low-disk / HuggingFace), mirroring Elementum's sliding RAM
// window. It is enabled only when Config.MemoryCacheSize > 0; the default path
// keeps storage.NewFileByInfoHash unchanged.
//
// Mechanism (proven against anacrolix/torrent v1.61.0):
//
// The backend combines two cooperating bounds that anacrolix is explicitly
// designed to support for capacity-limited storage:
//
//  1. Request bounding via TorrentImpl.Capacity. We hand anacrolix a single
//     shared *func() (cap int64, capped bool) returning (budget, true). The
//     request strategy (internal/request-strategy/order.go GetRequestablePieces)
//     walks pieces in priority order, subtracts each piece length from the
//     remaining capacity, and stops once the budget is exhausted. Because the
//     order is keyed on reader proximity, only the highest-priority pieces that
//     fit in the budget are ever requested; as a reader advances, the window
//     slides forward. Sharing one Capacity pointer across torrents makes the
//     budget global (torrent-piece-request-order.go keys the shared request
//     order on the Capacity pointer).
//
//  2. A hard byte cap enforced by this storage via LRU eviction of COMPLETE
//     pieces. On a write that would exceed the budget we free the least-
//     recently-accessed complete piece(s): drop the buffer and flip the piece
//     to not-complete. ReadAt updates recency, so recently-read pieces are
//     retained and already-played pieces are evicted first.
//
// Correctness — we never serve wrong or stale bytes:
//
//   - A non-resident piece reports Completion{Ok: true, Complete: false}, and its
//     ReadAt returns (0, errPieceEvicted), never (0, nil) — the storage.Piece /
//     storagePieceReader wrappers panic on a (0, nil) read.
//   - Evicting bytes is NOT enough on its own: anacrolix only clears a piece's
//     dirty-chunk bitmap on hash *failure*, never on completion, so a completed
//     piece keeps reading as "have" (reader.available stays > 0). reader.readAt
//     then spin-retries the failing read forever (a stack overflow) instead of
//     blocking, because it never sees the piece as unavailable. To close this we
//     drive a real re-download: ReadAt on an evicted piece calls the refetch hook
//     (wired by the manager to Torrent.Piece(i).VerifyData), which re-hashes the
//     empty piece, fails, clears its dirty chunks, and re-requests it — so the
//     reader blocks for the re-download. A short backoff on the evicted-read path
//     bounds anacrolix's retry recursion until that re-pend lands.
//   - We only ever evict COMPLETE pieces, so in-flight (partially written)
//     pieces are never dropped and anacrolix's chunk bookkeeping is never
//     invalidated mid-download.
//
// Bound vs. correctness: if every resident piece is still in-flight (rare; the
// Capacity request bound keeps the in-flight set small), eviction may find no
// complete victim and resident bytes briefly exceed the budget rather than
// dropping live data. This is the deliberate conservative direction — correct
// bytes always, with at most the bounded in-flight working set as overage.

import (
	"container/list"
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// errPieceEvicted is returned by ReadAt when a piece's bytes are not resident
// (never written, or evicted to stay within budget). Returning a non-nil error
// with n == 0 is required by the storage.Piece / storagePieceReader wrappers
// (which panic on a 0, nil read) and signals anacrolix to re-fetch the piece.
var errPieceEvicted = errors.New("engine: memstorage: piece not resident")

// memStorage is an opt-in storage.ClientImplCloser that keeps piece data in RAM
// bounded by a byte budget shared across all torrents it opens. It is safe for
// concurrent use: anacrolix calls ReadAt/WriteAt/Completion/MarkComplete on
// pieces concurrently across torrents.
type memStorage struct {
	capacity int64                   // hard byte budget across all torrents
	capFn    storage.TorrentCapacity // shared pointer handed to every torrent

	mu   sync.Mutex
	used int64      // resident bytes currently accounted (sum of resident piece lengths)
	lru  *list.List // front = most-recently-used, back = least; values are *memPiece

	// refetch, when set by the manager, forces anacrolix to re-download a piece
	// whose bytes were evicted. refetchBackoff briefly throttles the evicted-read
	// path so anacrolix's reader.readAt retry loop cannot recurse into a stack
	// overflow before the re-download is requested. Both are zero in the direct
	// unit tests, which drive the backend without a live torrent.
	refetch        func(ih metainfo.Hash, piece int)
	refetchBackoff time.Duration
}

var _ storage.ClientImplCloser = (*memStorage)(nil)

// newMemStorage returns an in-RAM storage backend bounded by capacity bytes,
// shared across every torrent opened on it.
func newMemStorage(capacity int64) *memStorage {
	s := &memStorage{
		capacity:       capacity,
		lru:            list.New(),
		refetchBackoff: 100 * time.Millisecond,
	}
	// One shared Capacity function for all torrents => one global budget. The
	// pointer identity is what anacrolix uses to share the request-order/budget
	// across torrents, so it must be the same pointer for every OpenTorrent.
	capFn := func() (int64, bool) { return s.capacity, true }
	s.capFn = &capFn
	return s
}

// OpenTorrent binds a new torrent to the shared RAM budget. Only Piece, Close
// and Capacity are set; anacrolix falls back from PieceWithHash to Piece.
func (s *memStorage) OpenTorrent(
	_ context.Context,
	info *metainfo.Info,
	ih metainfo.Hash,
) (storage.TorrentImpl, error) {
	mt := &memTorrent{
		store:    s,
		infoHash: ih,
		pieces:   make([]*memPiece, info.NumPieces()),
	}
	return storage.TorrentImpl{
		Piece:    mt.piece,
		Close:    mt.close,
		Capacity: s.capFn,
	}, nil
}

// Close drops all resident data. anacrolix calls each torrent's Close on Drop;
// this is the client-level Close (manager shutdown).
func (s *memStorage) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lru.Init()
	s.used = 0
	return nil
}

// touch marks a resident piece most-recently-used. Caller holds s.mu.
func (s *memStorage) touch(mp *memPiece) {
	if mp.elem != nil {
		s.lru.MoveToFront(mp.elem)
	}
}

// dropLocked evicts a resident piece: removes it from the LRU, frees its buffer,
// decrements the budget, and flips it to not-complete so a subsequent ReadAt
// fails and anacrolix re-fetches it. Caller holds s.mu.
func (s *memStorage) dropLocked(mp *memPiece) {
	if mp.elem != nil {
		s.lru.Remove(mp.elem)
		mp.elem = nil
	}
	if mp.data != nil {
		s.used -= mp.length
		mp.data = nil
	}
	mp.complete = false
}

// evictLocked frees least-recently-used COMPLETE pieces (never keep) until
// used+need fits within capacity, or no evictable piece remains. In-flight
// (incomplete) pieces are never dropped. Caller holds s.mu.
func (s *memStorage) evictLocked(need int64, keep *memPiece) {
	for s.used+need > s.capacity {
		var victim *memPiece
		// Scan from the least-recently-used end for the first complete piece.
		for el := s.lru.Back(); el != nil; el = el.Prev() {
			cand := el.Value.(*memPiece)
			if cand != keep && cand.complete {
				victim = cand
				break
			}
		}
		if victim == nil {
			return // nothing safe to evict; tolerate a transient overage
		}
		s.dropLocked(victim)
	}
}

// memTorrent is the per-torrent view onto the shared store. pieces is indexed by
// piece index and lazily populated; the slice never grows, so the stored
// pointers are stable. All access is guarded by store.mu.
type memTorrent struct {
	store    *memStorage
	infoHash metainfo.Hash
	pieces   []*memPiece
}

// piece returns the (lazily created) memPiece for p.
func (mt *memTorrent) piece(p metainfo.Piece) storage.PieceImpl {
	idx := p.Index()
	s := mt.store
	s.mu.Lock()
	defer s.mu.Unlock()
	mp := mt.pieces[idx]
	if mp == nil {
		mp = &memPiece{store: s, mt: mt, index: idx, length: p.Length()}
		mt.pieces[idx] = mp
	}
	return mp
}

// close drops every resident piece belonging to this torrent (called by
// anacrolix when the torrent is dropped).
func (mt *memTorrent) close() error {
	s := mt.store
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, mp := range mt.pieces {
		if mp != nil {
			s.dropLocked(mp)
		}
	}
	return nil
}

// memPiece holds one piece's bytes in RAM. All fields are guarded by store.mu.
type memPiece struct {
	store  *memStorage
	mt     *memTorrent // owning torrent (for infohash on evicted-read refetch)
	index  int         // piece index (for evicted-read refetch)
	length int64       // full length of this piece (bytes)

	data     []byte        // nil when not resident (never written or evicted)
	complete bool          // hash-verified by anacrolix and still resident
	elem     *list.Element // position in store.lru while resident; nil otherwise
}

var _ storage.PieceImpl = (*memPiece)(nil)

// WriteAt stores chunk bytes at off within the piece, allocating the full piece
// buffer (and making room in the budget) on first write.
func (mp *memPiece) WriteAt(b []byte, off int64) (int, error) {
	s := mp.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if off < 0 {
		return 0, errors.New("engine: memstorage: negative offset")
	}
	if off >= mp.length {
		return 0, io.EOF
	}
	if mp.data == nil {
		// First write: make room for this piece, then allocate it resident.
		// mp is not yet in the LRU, so eviction cannot target it.
		s.evictLocked(mp.length, mp)
		mp.data = make([]byte, mp.length)
		s.used += mp.length
		mp.elem = s.lru.PushFront(mp)
	} else {
		s.touch(mp)
	}
	n := copy(mp.data[off:], b)
	return n, nil
}

// ReadAt serves bytes from the resident buffer and marks the piece
// most-recently-used. A non-resident piece returns errPieceEvicted so anacrolix
// re-fetches it rather than reading stale data.
func (mp *memPiece) ReadAt(b []byte, off int64) (int, error) {
	s := mp.store
	s.mu.Lock()
	// Fast path: resident bytes in range. This is the ONLY way ReadAt returns a
	// non-zero count; every other outcome is a zero-byte "miss" handled below.
	if mp.data != nil && off >= 0 && off < mp.length {
		n := copy(b, mp.data[off:mp.length])
		s.touch(mp)
		s.mu.Unlock()
		if int64(n) < int64(len(b)) {
			// Filled to the end of the piece without satisfying the whole request.
			return n, io.EOF
		}
		return n, nil
	}
	refetch, backoff, ih, idx := s.refetch, s.refetchBackoff, mp.mt.infoHash, mp.index
	evicted := mp.data == nil
	s.mu.Unlock()

	// Any zero-byte read of a piece anacrolix may still consider present — its
	// bytes were evicted to stay within budget, or the requested offset is at/
	// past our piece end — would make anacrolix's reader.readAt spin-retry the
	// read forever under a capped storage (BEP "hasStorageCap" recursion), i.e.
	// a stack overflow, because the piece keeps reading as available. Force a
	// real re-download (VerifyData re-hashes the piece; for evicted bytes the
	// hash fails, which clears the piece's dirty chunks and re-requests it) and
	// rate-limit the retry so the reader blocks for the refetch instead of
	// recursing without bound. refetch is nil in the direct unit tests, which
	// then observe the bare error returns below.
	if refetch != nil {
		refetch(ih, idx)
		if backoff > 0 {
			time.Sleep(backoff)
		}
	}
	switch {
	case off < 0:
		return 0, errors.New("engine: memstorage: negative offset")
	case evicted:
		return 0, errPieceEvicted
	default:
		// Resident, but the requested offset is at/after the piece end.
		return 0, io.EOF
	}
}

// MarkComplete records that anacrolix verified the piece hash. The piece becomes
// eligible for eviction; we trim the budget now that a complete victim exists.
func (mp *memPiece) MarkComplete() error {
	s := mp.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if mp.data == nil {
		// Cannot be complete without resident data; should not happen because
		// anacrolix writes all chunks before marking complete.
		return errPieceEvicted
	}
	mp.complete = true
	s.touch(mp)
	s.evictLocked(0, mp)
	return nil
}

// MarkNotComplete flips the piece to incomplete. The buffer is kept: anacrolix
// calls this on a hash failure or chunk race and then rewrites the same offsets,
// so reusing the buffer avoids a reallocation and the bytes are overwritten
// before they could be read as complete.
func (mp *memPiece) MarkNotComplete() error {
	s := mp.store
	s.mu.Lock()
	defer s.mu.Unlock()
	mp.complete = false
	return nil
}

// Completion reports the piece state. Ok is always true: this storage is the
// definitive source of truth. Complete is true only while the verified bytes are
// still resident, so an evicted piece reads as not-complete and is re-fetched.
func (mp *memPiece) Completion() storage.Completion {
	s := mp.store
	s.mu.Lock()
	defer s.mu.Unlock()
	return storage.Completion{
		Ok:       true,
		Complete: mp.complete && mp.data != nil,
	}
}
