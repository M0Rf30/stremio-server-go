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
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// errPieceEvicted is returned by ReadAt when a piece's bytes are not resident
// (never written, or evicted to stay within budget). Returning a non-nil error
// with n == 0 is required by the storage.Piece / storagePieceReader wrappers
// (which panic on a 0, nil read) and signals anacrolix to re-fetch the piece.
var errPieceEvicted = errors.New("engine: memstorage: piece not resident")

// memBufPool maps piece length (int64) -> *sync.Pool of []byte.
// Keyed by length because the final piece usually has a different length than
// the rest; handing back an undersized buffer would corrupt the next writer.
var memBufPool sync.Map

// getPieceBuf returns a pooled, zero-or-reused byte slice of exactly length n.
// No zeroing is needed: anacrolix writes every chunk before MarkComplete, and
// ReadAt only serves data once the piece is complete.
func getPieceBuf(n int64) []byte {
	v, _ := memBufPool.LoadOrStore(n, &sync.Pool{New: func() any {
		b := make([]byte, n)
		return &b
	}})
	return *(v.(*sync.Pool).Get().(*[]byte))
}

// putPieceBuf returns b to the pool it was drawn from (keyed by cap).
// The caller must not use b after this call.
func putPieceBuf(b []byte) {
	if v, ok := memBufPool.Load(int64(cap(b))); ok {
		v.(*sync.Pool).Put(&b)
	}
}

// memStorage is an opt-in storage.ClientImplCloser that keeps piece data in RAM
// bounded by a byte budget shared across all torrents it opens. It is safe for
// concurrent use: anacrolix calls ReadAt/WriteAt/Completion/MarkComplete on
// pieces concurrently across torrents.
type memStorage struct {
	capacity int64                   // hard byte budget across all torrents
	capFn    storage.TorrentCapacity // shared pointer handed to every torrent

	// mu guards all mutable state below. Read-only operations (ReadAt,
	// Completion) take RLock so concurrent reads proceed in parallel; all
	// mutating operations take the write Lock.
	mu   sync.RWMutex
	used int64 // resident bytes currently accounted (sum of resident piece lengths)

	// completeSet tracks only the resident, hash-verified pieces. Eviction
	// iterates this set exclusively, avoiding in-flight pieces entirely.
	completeSet map[*memPiece]struct{}

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
		completeSet:    make(map[*memPiece]struct{}),
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
	// Pool complete-piece buffers on shutdown; incomplete pieces are handled by
	// each torrent's close which anacrolix should have called before this.
	for mp := range s.completeSet {
		if mp.data != nil {
			putPieceBuf(mp.data)
			mp.data = nil
			mp.complete = false
		}
	}
	s.completeSet = make(map[*memPiece]struct{})
	s.used = 0
	return nil
}

// dropLocked frees a resident piece: returns its buffer to the pool,
// decrements the budget, removes it from completeSet, and flips it to
// not-complete so a subsequent ReadAt fails and anacrolix re-fetches it.
// Caller holds s.mu (write lock).
func (s *memStorage) dropLocked(mp *memPiece) {
	if mp.data != nil {
		// Return buffer to pool; no zeroing needed — anacrolix writes all chunks
		// before MarkComplete and ReadAt only serves resident complete data.
		putPieceBuf(mp.data)
		s.used -= mp.length
		mp.data = nil
	}
	delete(s.completeSet, mp)
	mp.complete = false
}

// evictLocked frees COMPLETE pieces until used+need fits within capacity, or
// no evictable piece remains. Victim selection iterates completeSet only —
// in-flight (incomplete) pieces are never visited, eliminating the former O(n)
// LRU scan. The piece with the smallest lastUsed (unix-nanos, updated after each
// successful ReadAt) is the victim, preserving read-recency ordering.
// Caller holds s.mu (write lock).
func (s *memStorage) evictLocked(need int64, keep *memPiece) {
	for s.used+need > s.capacity {
		// Find the complete piece with the oldest lastUsed (least recently read).
		var victim *memPiece
		var oldest int64
		for cand := range s.completeSet {
			if cand == keep {
				continue
			}
			t := cand.lastUsed.Load()
			if victim == nil || t < oldest {
				victim = cand
				oldest = t
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

// memPiece holds one piece's bytes in RAM. data and complete are guarded by
// store.mu; lastUsed is updated lock-free via atomic after each successful read.
type memPiece struct {
	store  *memStorage
	mt     *memTorrent // owning torrent (for infohash on evicted-read refetch)
	index  int         // piece index (for evicted-read refetch)
	length int64       // full length of this piece (bytes)

	data     []byte // nil when not resident (never written or evicted)
	complete bool   // hash-verified by anacrolix and still resident

	// lastUsed records the unix-nanos timestamp of the most recent successful
	// ReadAt on this piece, updated atomically without holding s.mu. evictLocked
	// uses it to pick the least-recently-read complete piece as the eviction victim.
	lastUsed atomic.Int64
}

var _ storage.PieceImpl = (*memPiece)(nil)

// WriteAt stores chunk bytes at off within the piece, allocating the full piece
// buffer from the pool (and making room in the budget) on first write.
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
		// mp is not yet in completeSet (incomplete), so eviction cannot target it.
		s.evictLocked(mp.length, mp)
		mp.data = getPieceBuf(mp.length) // pooled; no zeroing needed (all chunks written before MarkComplete)
		s.used += mp.length
	}
	n := copy(mp.data[off:], b)
	return n, nil
}

// ReadAt serves bytes from the resident buffer. On the fast path it holds only
// an RLock, so multiple concurrent reads run in parallel. Recency (for eviction
// ordering) is recorded via an atomic store after releasing the RLock — no write
// lock contention on the read hot-path. A non-resident piece returns
// errPieceEvicted so anacrolix re-fetches it rather than reading stale data.
func (mp *memPiece) ReadAt(b []byte, off int64) (int, error) {
	s := mp.store
	s.mu.RLock()
	// Fast path: resident bytes in range. This is the ONLY way ReadAt returns a
	// non-zero count; every other outcome is a zero-byte "miss" handled below.
	if mp.data != nil && off >= 0 && off < mp.length {
		n := copy(b, mp.data[off:mp.length])
		s.mu.RUnlock()
		// Update recency outside the lock: concurrent reads proceed in parallel
		// and eviction uses lastUsed, not LRU MoveToFront. A tiny lag between the
		// copy and the stamp is acceptable for eviction ordering.
		mp.lastUsed.Store(time.Now().UnixNano())
		if int64(n) < int64(len(b)) {
			// Filled to the end of the piece without satisfying the whole request.
			return n, io.EOF
		}
		return n, nil
	}
	refetch, backoff, ih, idx := s.refetch, s.refetchBackoff, mp.mt.infoHash, mp.index
	evicted := mp.data == nil
	s.mu.RUnlock()

	// Handle non-eviction cases immediately — no refetch or sleep needed.
	if off < 0 {
		return 0, errors.New("engine: memstorage: negative offset")
	}
	if !evicted {
		// Resident piece, offset at/past piece end — legitimate EOF; the
		// read is satisfied and anacrolix will not spin on this path.
		// Returning immediately avoids a spurious VerifyData call and the
		// 100 ms backoff that were incorrectly applied to this branch.
		return 0, io.EOF
	}
	// Only the genuine evicted-piece case reaches here.  Force a
	// re-download: VerifyData re-hashes the (now empty) piece, the hash
	// fails, anacrolix clears the piece's dirty-chunk bitmap and
	// re-requests it, so reader.readAt blocks on the re-fetch instead of
	// recursing into a stack overflow.  The short sleep rate-limits the
	// retry until the refetch lands.
	if refetch != nil {
		refetch(ih, idx)
		if backoff > 0 {
			time.Sleep(backoff)
		}
	}
	return 0, errPieceEvicted
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
	// Stamp recency so this piece starts with a fair lastUsed for eviction;
	// older pieces (with smaller timestamps) will be evicted first.
	mp.lastUsed.Store(time.Now().UnixNano())
	s.completeSet[mp] = struct{}{}
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
	delete(s.completeSet, mp) // no longer eligible for eviction
	return nil
}

// Completion reports the piece state. Ok is always true: this storage is the
// definitive source of truth. Complete is true only while the verified bytes are
// still resident, so an evicted piece reads as not-complete and is re-fetched.
func (mp *memPiece) Completion() storage.Completion {
	s := mp.store
	s.mu.RLock()
	defer s.mu.RUnlock()
	return storage.Completion{
		Ok:       true,
		Complete: mp.complete && mp.data != nil,
	}
}
