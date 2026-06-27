// Package engine wraps anacrolix/torrent to provide the EngineManager and
// Engine interfaces declared in internal/types. It creates a single dual-stack
// (IPv4+IPv6, TCP+uTP, BEP32 DHT) torrent Client and exposes idempotent torrent
// management over it.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/mse"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/net/proxy"
	"golang.org/x/time/rate"

	"github.com/M0Rf30/stremio-server-go/internal/logging"
	"github.com/M0Rf30/stremio-server-go/internal/types"
)

// videoExts is the set of extensions considered "playable" for GuessFileIdx.
var videoExts = map[string]bool{
	".mkv":  true,
	".mp4":  true,
	".avi":  true,
	".mov":  true,
	".m4v":  true,
	".webm": true,
	".flv":  true,
	".wmv":  true,
	".mpg":  true,
	".mpeg": true,
	".ts":   true,
	".m2ts": true,
}

// speedSample holds a bandwidth checkpoint for delta-speed calculation.
type speedSample struct {
	at         time.Time
	downloaded int64
	uploaded   int64
}

// engine is a single torrent handle satisfying types.Engine.
type engine struct {
	t        *torrent.Torrent
	infoHash string // lower-cased
	path     string // cache path (reported in Stats.opts.path)

	mu               sync.Mutex
	last             speedSample
	lastDLSpeed      float64          // cached bytes/sec from last Stats(); used for readahead scaling
	selected         map[int]struct{} // file indices marked for background download
	primed           map[int]struct{} // file indices whose moov/header pieces are primed (once each)
	lastAccess       time.Time        // updated on NewReader/Stats; used by the janitor for LRU eviction
	openReaders      int              // active NewReader handles; >0 pins the torrent against eviction
	reading          map[int]int      // file idx -> open reader count; a file with readers is never demoted
	tailWarmed       map[int]struct{} // file idx -> tail (moov) actively pre-read once, to beat front starvation
	prefetched       map[int]struct{} // source file idx -> next-file boundary already prefetched (once each)
	onceMetaPriority sync.Once        // ensures boundary-piece prioritization runs exactly once

	// Announce-URL cache: refreshed at most once per annoTTL to avoid acquiring
	// the anacrolix Metainfo()/DistinctValues() locks on every Stats() call.
	annoMu     sync.Mutex
	annoExpiry time.Time
	cachedURLs []string
	cachedOpts types.Options // full Opts; rebuilt when cachedURLs are refreshed
}

// ensureDownloading marks file idx for full background download and demotes any
// previously-selected file that no longer has an open reader back to
// PiecePriorityNone. Streaming clients read one file at a time; without this,
// every file the user has ever clicked keeps downloading forever, so the
// available bandwidth fragments across all of them and the file actually being
// played cannot win its leading pieces — playback stalls even while the torrent
// downloads fast. Files with a live reader are never demoted.
func (e *engine) ensureDownloading(idx int) {
	if idx < 0 {
		return
	}
	files := e.t.Files()
	if idx >= len(files) {
		return
	}
	e.mu.Lock()
	if e.selected == nil {
		e.selected = map[int]struct{}{}
	}
	_, already := e.selected[idx]
	demote := make([]int, 0, len(e.selected))
	for i := range e.selected {
		if i != idx && e.reading[i] == 0 {
			demote = append(demote, i)
			delete(e.selected, i)
			delete(e.primed, i)
		}
	}
	if !already {
		e.selected[idx] = struct{}{}
		delete(e.primed, idx) // (re)prime the moov of the file now being played
	}
	e.mu.Unlock()

	for _, i := range demote {
		if i < len(files) {
			files[i].SetPriority(torrent.PiecePriorityNone)
		}
	}
	if !already {
		files[idx].Download()
	}
}

// manager owns the anacrolix Client and the live engine map, satisfying types.EngineManager.
type manager struct {
	client      *torrent.Client
	cfg         types.Config
	downLimiter *rate.Limiter // shared pointer in cc.DownloadRateLimiter; updated by SetLimitFn
	upLimiter   *rate.Limiter // shared pointer in cc.UploadRateLimiter; updated by SetLimitFn

	// memStorage is non-nil only when the opt-in in-RAM piece cache is active
	// (Config.MemoryCacheSize > 0). anacrolix does not Close a user-provided
	// DefaultStorage, so the manager owns closing it (see Close).
	memStorage io.Closer

	mu        sync.RWMutex
	engines   map[string]*engine // keyed by lower-cased infoHash
	done      chan struct{}      // closed by Close() to stop background goroutines
	closeOnce sync.Once          // ensures done is closed exactly once (idempotent Close)
	limitOnce sync.Once          // ensures SetLimitFn goroutine is started at most once

	// refetching dedupes in-flight VerifyData re-downloads of evicted in-RAM
	// pieces, keyed by refetchKey. Only exercised when the in-RAM cache is on.
	refetching sync.Map
}

// Compile-time interface satisfaction checks.
var _ types.EngineManager = (*manager)(nil)
var _ types.Engine = (*engine)(nil)

// refetchKey identifies an in-flight re-download of one evicted in-RAM piece.
type refetchKey struct {
	infoHash string
	piece    int
}

// refetchPiece forces anacrolix to re-download a piece whose in-RAM bytes were
// evicted. memStorage.ReadAt calls it when an evicted piece is read: VerifyData
// re-hashes the (now empty) piece, the hash fails, anacrolix clears the piece's
// dirty-chunk bitmap and re-requests it, and the reader blocks for the
// re-download instead of spinning reader.readAt into a stack overflow. Each
// (infohash, piece) verify runs at most once concurrently.
func (m *manager) refetchPiece(ih metainfo.Hash, piece int) {
	key := ih.HexString()
	m.mu.RLock()
	e := m.engines[key]
	m.mu.RUnlock()
	if e == nil || e.t == nil {
		return
	}
	if piece < 0 || piece >= e.t.NumPieces() {
		return
	}
	rk := refetchKey{infoHash: key, piece: piece}
	if _, inflight := m.refetching.LoadOrStore(rk, struct{}{}); inflight {
		return
	}
	go func() {
		defer m.refetching.Delete(rk)
		_ = e.t.Piece(piece).VerifyData()
	}()
}

// New creates a dual-stack anacrolix torrent Client and returns an EngineManager.
// ListenPort 0 lets the OS assign a port. CacheRoot is where piece data is stored.
func New(cfg types.Config) (types.EngineManager, error) {
	cc := torrent.NewDefaultClientConfig()

	// Dual-stack: IPv4 + IPv6, TCP + uTP. DHT (BEP32 v4+v6) is on by default.
	cc.DisableIPv4 = false
	cc.DisableIPv6 = false
	cc.DisableUTP = false
	cc.DisableTCP = false

	// Maximize peer sourcing + feature set.
	cc.DisablePEX = false                        // BEP11 peer exchange
	cc.DisableWebtorrent = cfg.DisableWebtorrent // WebRTC/WebTorrent peers (pion); gated by STREMIO_DISABLE_WEBTORRENT
	cc.DisableWebseeds = false                   // BEP19 HTTP webseeds
	cc.Seed = true                               // keep uploading while/after streaming (swarm health; uses IPv6 inbound)
	cc.AcceptPeerConnections = true
	cc.DisableAcceptRateLimiting = false // rate-limit inbound accepts to bound connection-handling CPU
	// Disable tracker announces when requested — avoids the upstream anacrolix tracker/udp
	// data race in tests and suppresses network fetches in private/DHT-only mode.
	cc.DisableTrackers = cfg.DisableTrackers

	// Silence anacrolix's benign context-canceled reader ERROR spam (player
	// seeks/disconnects and our background warm/prefetch cancellations) while
	// preserving genuine errors and the existing log format.
	cc.Slogger = slog.New(newReadCancelFilter(logging.For("torrent").Handler()))

	// Connection budgets tuned for streaming: enough peers for full throughput
	// without excessive MSE/RC4 handshakes and dial churn, which are the dominant
	// engine CPU cost on large, fast swarms (4K). Scaled by STREMIO_PEERS_PER_TORRENT
	// (cfg.PeersPerTorrent); default 0 keeps the historical 50/25/500 ratios.
	est, half, high := peerBudget(cfg.PeersPerTorrent)
	cc.EstablishedConnsPerTorrent = est
	cc.HalfOpenConnsPerTorrent = half
	cc.TorrentPeersHighWater = high

	// Honor the documented contract: 0 = OS-assigned (random) port. anacrolix's
	// NewDefaultClientConfig hardcodes 42069, so we must set it unconditionally
	// or two instances (and the test suite) collide on that port.
	cc.ListenPort = cfg.ListenPort

	// Storage backend. Default (MemoryCacheSize == 0): file storage partitioned
	// by info-hash so torrents don't collide on disk. Opt-in: a bounded in-RAM
	// backend that never writes piece data to disk (mobile/Termux/low-disk/
	// HuggingFace). The mem backend is a ClientImplCloser; anacrolix only closes
	// the default storage it creates itself, never a provided one, so the manager
	// closes it (see Close).
	var memStorageCloser io.Closer
	var memStore *memStorage
	if cfg.MemoryCacheSize > 0 {
		ms := newMemStorage(cfg.MemoryCacheSize)
		cc.DefaultStorage = ms
		memStorageCloser = ms
		memStore = ms
		logging.For("engine").Info("in-RAM piece cache enabled; piece data not written to disk", "bytes", cfg.MemoryCacheSize)
	} else {
		cc.DefaultStorage = storage.NewFileByInfoHash(cfg.CacheRoot)
		logging.For("engine").Info("disk piece cache", "path", cfg.CacheRoot)
	}

	// Bandwidth rate limiters — start unlimited; SetLimitFn() adjusts them live.
	// We keep the *rate.Limiter pointers so we can call SetLimit/SetBurst without
	// changing the pointer stored in cc (anacrolix does not allow pointer replacement
	// after client creation). Burst for download: ≥1 MiB (anacrolix default floor);
	// for upload: ≥512 KiB (largest peer-request chunk size we might serve).
	downLimiter := rate.NewLimiter(rate.Inf, 1<<22) // 4 MiB burst, unlimited
	upLimiter := rate.NewLimiter(rate.Inf, 1<<22)   // 4 MiB burst, unlimited
	cc.DownloadRateLimiter = downLimiter
	cc.UploadRateLimiter = upLimiter

	// Censorship resistance / anonymity (all default = anacrolix default behavior)
	applyBTEncryption(cc, cfg.BTEncryption)
	if err := applyBTProxy(cc, cfg.BTProxy); err != nil {
		return nil, fmt.Errorf("engine: bt proxy: %w", err)
	}
	applyDHTBootstrap(cc, cfg.DHTBootstrap)
	cc.AnonymousMode = cfg.BTAnonymous
	if cfg.BTAnonymous {
		logging.For("engine").Info("bittorrent anonymous mode enabled")
	}

	client, err := torrent.NewClient(cc)
	if err != nil {
		return nil, fmt.Errorf("engine: create torrent client: %w", err)
	}

	// Create the done channel first so initTrackers can stop its 24h goroutine
	// when Close() is called.
	done := make(chan struct{})

	// Load curated trackers (cached) and refresh + rank from upstream in the background.
	// Skip tracker list fetch when announces are disabled (tests / private mode).
	if !cfg.DisableTrackers {
		initTrackers(cfg.CacheRoot, cfg.TrackersMax, cfg.TrackersURL, cfg.BTProxy, done)
	}

	m := &manager{
		client:      client,
		cfg:         cfg,
		downLimiter: downLimiter,
		upLimiter:   upLimiter,
		engines:     make(map[string]*engine),
		done:        done,
		memStorage:  memStorageCloser,
	}
	// Wire the evicted-piece re-download hook now that the manager (and its
	// engine map) exists. No torrents are added until EnsureEngine, so this is
	// set before any read can occur.
	if memStore != nil {
		memStore.refetch = m.refetchPiece
	}
	return m, nil
}

// --------------------------------------------------------------------------
// EngineManager implementation
// --------------------------------------------------------------------------

// EnsureEngine returns the existing engine for infoHash or creates one.
// A second call with additional trackers merges them without restarting.
func (m *manager) EnsureEngine(infoHash string, opts types.AddOptions) (types.Engine, error) {
	ih := strings.ToLower(infoHash)

	// Fast path: already exists.
	m.mu.RLock()
	if e, ok := m.engines[ih]; ok {
		m.mu.RUnlock()
		mergeTrackers(e.t, opts, !m.cfg.DisableWebtorrent)
		return e, nil
	}
	m.mu.RUnlock()

	// Slow path: acquire write lock, re-check (double-check locking), then add.
	m.mu.Lock()
	defer m.mu.Unlock()

	if e, ok := m.engines[ih]; ok {
		mergeTrackers(e.t, opts, !m.cfg.DisableWebtorrent)
		return e, nil
	}

	var (
		t   *torrent.Torrent
		err error
	)

	if opts.MetaInfo != nil {
		// Raw .torrent bytes provided — parse and add.
		mi, miErr := metainfo.Load(bytes.NewReader(opts.MetaInfo))
		if miErr != nil {
			return nil, fmt.Errorf("engine: parse metainfo for %s: %w", ih, miErr)
		}
		// Drop ws/wss (and any non-http(s)/udp) announces when WebTorrent is
		// disabled; otherwise anacrolix's regular dispatcher panics on them.
		allowWS := !m.cfg.DisableWebtorrent
		mi.Announce = strings.Join(announceableTrackers([]string{mi.Announce}, allowWS), "")
		mi.AnnounceList = announceableTiers(mi.AnnounceList, allowWS)
		t, err = m.client.AddTorrent(mi)
	} else {
		// Only the info-hash is known; start with a plain magnet URI.
		magnet := "magnet:?xt=urn:btih:" + ih
		t, err = m.client.AddMagnet(magnet)
	}
	if err != nil {
		return nil, fmt.Errorf("engine: add torrent %s: %w", ih, err)
	}

	mergeTrackers(t, opts, !m.cfg.DisableWebtorrent)
	// Inject a baseline public-tracker list (like the official server) so bare /
	// trackerless magnets still find peers instead of relying on DHT alone.
	t.AddTrackers([][]string{announceableTrackers(getTrackers(), !m.cfg.DisableWebtorrent)})

	e := &engine{t: t, infoHash: ih, path: filepath.Join(m.cfg.CacheRoot, ih), lastAccess: time.Now()}
	m.engines[ih] = e

	// Spawn a one-shot goroutine that boosts container-metadata (moov-atom /
	// codec-init) reads once the torrent info dict is available. This is safe
	// to start inside the write lock because the goroutine only blocks on GotInfo.
	go applyBoundaryPriorities(e)

	return e, nil
}

// GetEngine returns the engine for infoHash if it exists.
func (m *manager) GetEngine(infoHash string) (types.Engine, bool) {
	ih := strings.ToLower(infoHash)
	m.mu.RLock()
	defer m.mu.RUnlock()
	if e, ok := m.engines[ih]; ok {
		return e, true
	}
	return nil, false
}

// RemoveEngine stops and removes the torrent identified by infoHash.
func (m *manager) RemoveEngine(infoHash string) error {
	ih := strings.ToLower(infoHash)
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.engines[ih]
	if !ok {
		return nil // idempotent
	}
	e.t.Drop()
	delete(m.engines, ih)
	return nil
}

// RemoveAll stops and removes all active torrents.
func (m *manager) RemoveAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for ih, e := range m.engines {
		e.t.Drop()
		delete(m.engines, ih)
	}
}

// ListEngines returns the lower-cased info-hashes of all active engines.
func (m *manager) ListEngines() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.engines))
	for ih := range m.engines {
		out = append(out, ih)
	}
	return out
}

// NumTorrents returns the number of currently active torrent engines.
func (m *manager) NumTorrents() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.engines)
}

// AllStats returns torrent-level stats (idx=-1) keyed by lower-cased infoHash.
func (m *manager) AllStats() map[string]*types.Stats {
	m.mu.RLock()
	snap := make(map[string]*engine, len(m.engines))
	for ih, e := range m.engines {
		snap[ih] = e
	}
	m.mu.RUnlock()

	result := make(map[string]*types.Stats, len(snap))
	for ih, e := range snap {
		result[ih] = e.Stats(-1)
	}
	return result
}

// Close shuts down the anacrolix client (drops all torrents, closes sockets).
// It also signals the janitor goroutine (if running) to stop.
func (m *manager) Close() error {
	m.closeOnce.Do(func() { close(m.done) })
	m.client.Close()
	if m.memStorage != nil {
		if err := m.memStorage.Close(); err != nil {
			logging.For("engine").Error("closing in-RAM piece cache", "err", err)
		}
	}
	return nil
}

// StartJanitor launches a background goroutine that evicts least-recently-used
// engines whenever the on-disk cache exceeds the budget returned by cacheSizeFn.
// The goroutine stops when Close() is called (which closes m.done).
func (m *manager) StartJanitor(cacheSizeFn func() int64) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.evict(cacheSizeFn())
				m.evictIdle(m.cfg.IdleTimeout)
			case <-m.done:
				return
			}
		}
	}()
}

// SetLimitFn wires live download/upload bandwidth limits from a settings getter.
// fn returns (downBytesPerSec, upBytesPerSec); 0 = unlimited, positive = cap in bytes/sec.
// For upload, callers should return 1 to indicate "effectively disabled" when seeding
// is turned off — this keeps the burst valid while making actual seeding negligible.
// A background goroutine (stopped on Close) polls fn every 5 s and adjusts the
// rate.Limiter instances that were baked into the anacrolix ClientConfig at startup.
func (m *manager) SetLimitFn(fn func() (downBytesPerSec, upBytesPerSec int64)) {
	applyLimit := func(limiter *rate.Limiter, n int64, isUpload bool) {
		if n <= 0 {
			// 0 or negative → unlimited
			limiter.SetLimit(rate.Inf)
			limiter.SetBurst(1 << 22) // 4 MiB
		} else {
			limiter.SetLimit(rate.Limit(n))
			// Burst must be at least as large as the biggest single Read/Write the
			// underlying layer performs. For download that's ~1 MiB (HTTP/2 frame);
			// for upload the peer-request chunk is 16 KiB. Provide headroom.
			burst := int(min(max(n, 1<<20), math.MaxInt32))
			if isUpload {
				burst = int(min(max(n, 1<<17), math.MaxInt32)) // 128 KiB floor for upload
			}
			limiter.SetBurst(burst)
		}
	}
	m.limitOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					down, up := fn()
					applyLimit(m.downLimiter, down, false)
					applyLimit(m.upLimiter, up, true)
				case <-m.done:
					return
				}
			}
		}()
	})
}

// evict removes the least-recently-used engines until total on-disk cache usage
// is within budget. budget <= 0 means unlimited — no eviction is performed.
//
// Lock ordering: sizes are measured without any lock held; the write lock is
// acquired only for individual map mutations, minimising contention with readers.
func (m *manager) evict(budget int64) {
	if budget <= 0 {
		return // 0 / nil cacheSize => unlimited
	}

	const graceWindow = 90 * time.Second

	// Snapshot engine pointers under read lock (avoids holding the lock during
	// expensive filesystem walks).
	m.mu.RLock()
	snap := make([]*engine, 0, len(m.engines))
	for _, e := range m.engines {
		snap = append(snap, e)
	}
	m.mu.RUnlock()

	if len(snap) == 0 {
		return
	}

	// Single filesystem walk of the cache root — attribute sizes to engine
	// subdirectories. This replaces N separate WalkDir calls (one per engine)
	// with one pass, reducing inode pressure on every janitor tick.
	type entry struct {
		e           *engine
		size        int64
		lastAccess  time.Time
		openReaders int
	}
	dirSizes := make(map[string]int64, len(snap))
	_ = filepath.WalkDir(m.cfg.CacheRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(m.cfg.CacheRoot, path)
		if rerr != nil {
			return nil
		}
		// First path component is the info-hash subdirectory.
		ih := rel
		if i := strings.IndexByte(rel, byte(filepath.Separator)); i >= 0 {
			ih = rel[:i]
		}
		if info, err2 := d.Info(); err2 == nil {
			dirSizes[ih] += info.Size()
		}
		return nil
	})

	entries := make([]entry, 0, len(snap))
	var total int64
	for _, e := range snap {
		e.mu.Lock()
		la := e.lastAccess
		or := e.openReaders
		e.mu.Unlock()
		sz := dirSizes[e.infoHash]
		entries = append(entries, entry{e: e, size: sz, lastAccess: la, openReaders: or})
		total += sz
	}

	if total <= budget {
		return
	}

	// Sort ascending by lastAccess (oldest first). The MRU engine is last and
	// is always preserved — we iterate only up to len(entries)-1.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].lastAccess.Before(entries[j].lastAccess)
	})

	now := time.Now()
	for i := 0; i < len(entries)-1 && total > budget; i++ {
		ent := entries[i]
		if ent.openReaders > 0 || now.Sub(ent.lastAccess) < graceWindow {
			// Actively streaming or recently accessed — never evict.
			continue
		}
		ih := ent.e.infoHash
		// Re-check under write lock: compare pointer identity, not just key
		// existence. A concurrent Remove+EnsureEngine may have replaced the
		// engine instance; evicting its snapshot would delete a live engine's
		// map entry and on-disk cache.
		m.mu.Lock()
		if m.engines[ih] != ent.e {
			m.mu.Unlock()
			continue // already removed or replaced by a concurrent caller
		}
		// Re-check under the write lock: a reader may have opened since the
		// snapshot. Closes the snapshot->Drop race so a live stream is never
		// closed out from under its HTTP reader. Lock order m.mu -> e.mu.
		ent.e.mu.Lock()
		pinned := ent.e.openReaders > 0
		ent.e.mu.Unlock()
		if pinned {
			m.mu.Unlock()
			continue
		}
		ent.e.t.Drop()
		delete(m.engines, ih)
		m.mu.Unlock()
		// Remove cached data from disk outside the write lock.
		_ = os.RemoveAll(ent.e.path)
		total -= ent.size
		logging.For("engine").Info("evicted torrent", "info_hash", ih, "freed_bytes", ent.size, "budget_bytes", budget)
	}
}

// evictIdle drops torrents that have had no open stream readers and no access
// for longer than idle. idle <= 0 disables it. This mirrors the official
// server's inactive-torrent reclaim (~5 min) so a stopped torrent is dropped
// even when cacheSize is unlimited (the size-based evict never fires then),
// while still keeping it alive long enough for instant scrub/resume/next-episode.
//
// Lock ordering matches evict (m.mu -> e.mu); the engine is re-checked under the
// write lock so a reader that opened since the snapshot is never dropped.
func (m *manager) evictIdle(idle time.Duration) {
	if idle <= 0 {
		return
	}

	m.mu.RLock()
	snap := make([]*engine, 0, len(m.engines))
	for _, e := range m.engines {
		snap = append(snap, e)
	}
	m.mu.RUnlock()

	now := time.Now()
	for _, e := range snap {
		e.mu.Lock()
		idleFor := now.Sub(e.lastAccess)
		pinned := e.openReaders > 0
		e.mu.Unlock()
		if pinned || idleFor < idle {
			continue
		}
		ih := e.infoHash
		m.mu.Lock()
		if m.engines[ih] != e {
			m.mu.Unlock()
			continue // already removed or replaced by a concurrent caller
		}
		// Re-check under the write lock: a reader may have opened since the
		// snapshot. Lock order m.mu -> e.mu.
		e.mu.Lock()
		stillPinned := e.openReaders > 0 || now.Sub(e.lastAccess) < idle
		e.mu.Unlock()
		if stillPinned {
			m.mu.Unlock()
			continue
		}
		e.t.Drop()
		delete(m.engines, ih)
		m.mu.Unlock()
		_ = os.RemoveAll(e.path)
		logging.For("engine").Info("removed idle torrent", "info_hash", ih, "idle_for", idleFor.Round(time.Second).String())
	}
}

// --------------------------------------------------------------------------
// Engine implementation
// --------------------------------------------------------------------------

// InfoHash returns the lower-cased hex info-hash string.
func (e *engine) InfoHash() string {
	return e.infoHash
}

// Ready blocks until torrent metadata is available or ctx is done.
func (e *engine) Ready(ctx context.Context) error {
	select {
	case <-e.t.GotInfo():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// hasInfo reports whether metadata (the info dict) is already available without blocking.
func (e *engine) hasInfo() bool {
	select {
	case <-e.t.GotInfo():
		return true
	default:
		return false
	}
}

// Files returns the list of files in the torrent. Returns nil before metadata is ready.
func (e *engine) Files() []types.FileInfo {
	if !e.hasInfo() {
		return nil
	}
	files := e.t.Files()
	out := make([]types.FileInfo, len(files))
	for i, f := range files {
		// DisplayPath is the slash-joined relative path; for single-file
		// torrents it returns the info.Name directly.
		name := f.DisplayPath()
		out[i] = types.FileInfo{
			Name:   name,
			Path:   name,
			Length: f.Length(),
			Offset: f.Offset(),
		}
	}
	return out
}

// NewReader returns a streaming, seek-capable reader for the file at idx. The
// readahead window starts small (8 MiB) so playback begins quickly, then grows
// with measured throughput up to 64 MiB for a deep buffer (see pinnedReader).
func (e *engine) NewReader(idx int) (io.ReadSeekCloser, int64, error) {
	// Mark this engine as recently active before any blocking work.
	e.mu.Lock()
	e.lastAccess = time.Now()
	cachedSpeed := e.lastDLSpeed // bytes/sec from last Stats(); 0 before first call
	e.mu.Unlock()
	if !e.hasInfo() {
		return nil, 0, fmt.Errorf("engine: metadata not yet available for %s", e.infoHash)
	}
	files := e.t.Files()
	if idx < 0 || idx >= len(files) {
		return nil, 0, fmt.Errorf("engine: file index %d out of range [0,%d)", idx, len(files))
	}
	f := files[idx]
	e.mu.Lock()
	e.openReaders++
	if e.reading == nil {
		e.reading = map[int]int{}
	}
	e.reading[idx]++
	e.mu.Unlock()
	e.ensureDownloading(idx) // fetch the file being played; demote abandoned ones
	e.primeBoundary(idx)     // top-priority the header/moov so playback starts fast
	e.warmMoov(idx)          // pre-read the tail so the moov downloads with the front
	e.prefetchNext(idx)      // opportunistically warm the next episode's header (binge UX)
	r := f.NewReader()
	r.SetReadahead(readaheadFor(cachedSpeed))
	now := time.Now()
	return &pinnedReader{
		tr:        r,
		e:         e,
		idx:       idx,
		deadline:  now.Add(startupReadTimeout),
		nextRAAdj: now.Add(readaheadAdjustInterval),
	}, f.Length(), nil
}

// warmMoov pre-fetches the bytes a player needs to start before they would
// arrive on their own. Non-faststart MP4s (e.g. most RARBG encodes) and MKVs
// keep their index (moov atom / Cues) at the END of the file; primeBoundary
// marks those pieces top-priority, but within equal priority the actively-read
// front wins, so the index trickles in last and playback stalls for a long time
// even while the torrent downloads fast. warmMoov opens a dedicated reader on
// the last few MiB and drains it so the index gets real demand and arrives
// alongside the header. This is the anacrolix equivalent of qBittorrent's
// "download first and last piece first" / Elementum's start-end buffering.
//
// Faststart MP4s already carry the moov at the front, so the main stream read
// covers it — warming the tail would only steal bandwidth and delay start, so
// it is skipped. Runs once per file; bounded by a timeout so a tail that never
// arrives (dead swarm) cannot leak the goroutine; tiny files are ignored.
func (e *engine) warmMoov(idx int) {
	if idx < 0 || !e.hasInfo() {
		return
	}
	e.mu.Lock()
	if e.tailWarmed == nil {
		e.tailWarmed = map[int]struct{}{}
	}
	if _, ok := e.tailWarmed[idx]; ok {
		e.mu.Unlock()
		return
	}
	e.tailWarmed[idx] = struct{}{}
	e.mu.Unlock()

	files := e.t.Files()
	if idx >= len(files) {
		return
	}
	f := files[idx]
	const minWarmSize int64 = 16 << 20 // ignore subtitles/samples/tiny files
	const tailWindow int64 = 8 << 20   // covers the moov atom of typical videos
	if f.Length() < minWarmSize {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if !needsTailWarm(ctx, f) {
			return // faststart MP4: index already at the front
		}
		r := f.NewReader()
		defer func() { _ = r.Close() }()
		r.SetReadahead(tailWindow)
		off := f.Length() - tailWindow
		if off < 0 {
			off = 0
		}
		if _, err := r.Seek(off, io.SeekStart); err != nil {
			return
		}
		buf := make([]byte, 64<<10)
		for {
			n, err := r.ReadContext(ctx, buf) // bounded; errors on timeout or torrent drop
			if n == 0 || err != nil {
				return
			}
		}
	}()
}

// needsTailWarm walks the leading MP4 box headers and reports false only when it
// confirms a faststart layout (a moov box appears before any mdat). For mdat
// first (moov at the end), non-MP4 containers (e.g. MKV, whose Cues live at the
// end), or anything it cannot parse, it returns true so the tail is warmed — the
// safe default. Reads are bounded by ctx.
func needsTailWarm(ctx context.Context, f *torrent.File) bool {
	r := f.NewReader()
	defer func() { _ = r.Close() }()
	r.SetReadahead(256 << 10)
	hdr := make([]byte, 8)
	var pos int64
	for i := 0; i < 6; i++ {
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			return true
		}
		if err := readFullCtx(ctx, r, hdr); err != nil {
			return true
		}
		size := int64(hdr[0])<<24 | int64(hdr[1])<<16 | int64(hdr[2])<<8 | int64(hdr[3])
		switch string(hdr[4:8]) {
		case "moov":
			return false // faststart: index before media data
		case "mdat":
			return true // media first: moov is at the end
		case "ftyp", "free", "skip", "wide", "pdin":
			// header/filler box; advance to the next one
		default:
			return true // not a recognizable faststart MP4 (e.g. MKV)
		}
		if size < 8 {
			return true // 64-bit/edge size: bail to the safe default
		}
		pos += size
	}
	return true
}

// readFullCtx fills b from r, honouring ctx for cancellation/timeout.
func readFullCtx(ctx context.Context, r torrent.Reader, b []byte) error {
	for got := 0; got < len(b); {
		n, err := r.ReadContext(ctx, b[got:])
		got += n
		if err != nil {
			return err
		}
	}
	return nil
}

// Streaming readahead bounds + timing.
const (
	minReadahead            int64         = 8 << 20          // 8 MiB floor — fast start
	maxReadahead            int64         = 64 << 20         // 64 MiB ceiling — deep buffer on fast swarms
	readaheadAdjustInterval time.Duration = 3 * time.Second  // how often a live reader recomputes its window
	startupReadTimeout      time.Duration = 90 * time.Second // bound the first read so a dead swarm errors instead of hanging
)

// readaheadFor returns the streaming readahead window for a measured download
// speed: ~2 s of throughput, clamped to [minReadahead, maxReadahead].
func readaheadFor(speed float64) int64 {
	ra := minReadahead
	if speed > 0 {
		if s := int64(2 * speed); s > ra {
			ra = s
		}
	}
	if ra > maxReadahead {
		ra = maxReadahead
	}
	return ra
}

// pinnedReader wraps a torrent file reader. While it is open the engine's
// openReaders count is >0 so the janitor never evicts the torrent mid-stream
// (Close decrements exactly once). It also (a) grows the readahead window as the
// measured speed rises — small at first for a fast start, deeper later for
// smooth playback — and (b) bounds the FIRST read with a deadline so a dead
// swarm returns an error instead of hanging the player forever.
type pinnedReader struct {
	tr   torrent.Reader
	e    *engine
	idx  int
	once sync.Once

	mu        sync.Mutex
	started   bool      // a read has returned data; later reads block normally
	deadline  time.Time // startup read deadline
	nextRAAdj time.Time // next readahead recompute time
}

func (p *pinnedReader) Read(b []byte) (int, error) {
	p.adjustReadahead()
	p.mu.Lock()
	started, dl := p.started, p.deadline
	p.mu.Unlock()
	if started {
		return p.tr.Read(b)
	}
	ctx, cancel := context.WithDeadline(context.Background(), dl)
	defer cancel()
	n, err := p.tr.ReadContext(ctx, b)
	if n > 0 {
		p.mu.Lock()
		p.started = true
		p.mu.Unlock()
	}
	return n, err
}

func (p *pinnedReader) Seek(offset int64, whence int) (int64, error) {
	return p.tr.Seek(offset, whence)
}

func (p *pinnedReader) Close() error {
	err := p.tr.Close()
	p.once.Do(func() {
		var demote bool
		p.e.mu.Lock()
		p.e.openReaders--
		if p.e.reading[p.idx] > 0 {
			p.e.reading[p.idx]--
		}
		// Last reader for this file is gone: stop greedily downloading the rest
		// of it. Without this the file stays marked Download() and keeps pulling
		// pieces to completion in the background after the user pressed stop. The
		// torrent itself stays connected (only the janitor's idle/LRU passes drop
		// it), so scrub/resume/next-episode are still instant — reopening the file
		// re-marks it via ensureDownloading + primeBoundary.
		if p.e.reading[p.idx] == 0 {
			delete(p.e.selected, p.idx)
			delete(p.e.primed, p.idx)
			demote = true
		}
		p.e.mu.Unlock()
		if demote {
			files := p.e.t.Files()
			if p.idx >= 0 && p.idx < len(files) {
				files[p.idx].SetPriority(torrent.PiecePriorityNone)
			}
		}
	})
	return err
}

// adjustReadahead recomputes the window from the latest measured speed, at most
// once per readaheadAdjustInterval, so the buffer deepens as throughput rises.
func (p *pinnedReader) adjustReadahead() {
	now := time.Now()
	p.mu.Lock()
	if now.Before(p.nextRAAdj) {
		p.mu.Unlock()
		return
	}
	p.nextRAAdj = now.Add(readaheadAdjustInterval)
	p.mu.Unlock()
	p.e.mu.Lock()
	sp := p.e.lastDLSpeed
	p.e.mu.Unlock()
	p.tr.SetReadahead(readaheadFor(sp))
}

// prefetchNext opportunistically warms the NEXT video file's header and tail
// (moov) at low (Normal) priority, so advancing to the next episode of a season
// pack starts fast. It touches only a few boundary pieces (not the whole file)
// and never raises them above the active stream's read window, so it uses only
// spare bandwidth and cannot starve current playback. Runs once per source file;
// the prefetch target is not added to e.selected, so demotion never touches it.
func (e *engine) prefetchNext(idx int) {
	if idx < 0 || !e.hasInfo() {
		return
	}
	e.mu.Lock()
	if e.prefetched == nil {
		e.prefetched = map[int]struct{}{}
	}
	if _, ok := e.prefetched[idx]; ok {
		e.mu.Unlock()
		return
	}
	e.prefetched[idx] = struct{}{}
	e.mu.Unlock()

	next := e.nextVideoIdx(idx)
	if next < 0 {
		return
	}
	files := e.t.Files()
	f := files[next]
	begin := f.BeginPieceIndex()
	end := f.EndPieceIndex()
	const prefetchPieces = 4 // header + index only, opportunistic
	for i := begin; i < begin+prefetchPieces && i < end; i++ {
		e.t.Piece(i).SetPriority(torrent.PiecePriorityNormal)
	}
	tailStart := end - prefetchPieces
	if tailStart < begin {
		tailStart = begin
	}
	for i := tailStart; i < end; i++ {
		e.t.Piece(i).SetPriority(torrent.PiecePriorityNormal)
	}
	logging.For("engine").Debug("prefetched next-file boundary", "info_hash", e.infoHash, "file_idx", next)
}

// nextVideoIdx returns the index of the first video file after idx, or -1.
func (e *engine) nextVideoIdx(idx int) int {
	files := e.t.Files()
	for i := idx + 1; i < len(files); i++ {
		if videoExts[strings.ToLower(filepath.Ext(files[i].DisplayPath()))] {
			return i
		}
	}
	return -1
}

// GuessFileIdx returns the index of the largest video file, falling back to the
// largest file overall, or -1 if the torrent has no files yet.
func (e *engine) GuessFileIdx() int {
	if !e.hasInfo() {
		return -1
	}
	files := e.t.Files()
	if len(files) == 0 {
		return -1
	}

	bestVideo := -1
	bestVideoLen := int64(-1)
	bestAny := 0
	bestAnyLen := int64(-1)

	for i, f := range files {
		l := f.Length()
		ext := strings.ToLower(filepath.Ext(f.DisplayPath()))
		if videoExts[ext] && l > bestVideoLen {
			bestVideoLen = l
			bestVideo = i
		}
		if l > bestAnyLen {
			bestAnyLen = l
			bestAny = i
		}
	}

	if bestVideo >= 0 {
		return bestVideo
	}
	return bestAny
}

// Stats returns a types.Stats snapshot. When idx >= 0 the per-file stream
// fields (StreamLen, StreamName, StreamProgress) are also populated.
// Download/upload speeds are computed from byte deltas between successive calls.
func (e *engine) Stats(idx int) *types.Stats {
	ts := e.t.Stats()

	// Bandwidth counters — ConnStats is embedded directly in TorrentStats →
	// AllConnStats → ConnStats, so field access is ts.BytesReadData.Int64().
	dl := ts.BytesReadData.Int64()
	ul := ts.BytesWrittenData.Int64()

	now := time.Now()
	e.mu.Lock()
	var dlSpeed, ulSpeed float64
	if !e.last.at.IsZero() {
		dt := now.Sub(e.last.at).Seconds()
		if dt > 0 {
			dlSpeed = float64(dl-e.last.downloaded) / dt
			ulSpeed = float64(ul-e.last.uploaded) / dt
		}
	}
	e.last = speedSample{at: now, downloaded: dl, uploaded: ul}
	e.lastAccess = now // update LRU timestamp inside the already-held lock
	if dlSpeed > 0 {
		e.lastDLSpeed = dlSpeed // cache for NewReader readahead scaling
	}
	e.mu.Unlock()

	// Clamp negative speeds (can happen when the client resets counters).
	if dlSpeed < 0 {
		dlSpeed = 0
	}
	if ulSpeed < 0 {
		ulSpeed = 0
	}

	// Peer gauges — embedded via TorrentGauges.
	activePeers := ts.ActivePeers
	totalPeers := ts.TotalPeers

	files := e.Files()
	// Refresh the announce-URL / Opts cache when the TTL has elapsed.
	// Metainfo() and DistinctValues() each acquire anacrolix internal locks; at
	// ~1 Hz polling from stremio-web those contend heavily. We memoize the
	// announce list and the full Opts map (which is otherwise allocated fresh
	// on every call) and refresh at most once every 30 s.
	const annoTTL = 30 * time.Second
	e.annoMu.Lock()
	if now.After(e.annoExpiry) {
		mi := e.t.Metainfo()
		urls := mi.UpvertedAnnounceList().DistinctValues()
		peerSrcs := make([]string, 0, 1+len(urls))
		peerSrcs = append(peerSrcs, "dht:"+e.infoHash)
		peerSrcs = append(peerSrcs, urls...)
		e.cachedURLs = urls
		e.cachedOpts = types.Options{
			DHT:        true,
			Growler:    types.Growler{Flood: 0},
			Path:       e.path,
			PeerSearch: types.PeerSearch{Min: 40, Max: 200, Sources: peerSrcs},
			Tracker:    true,
		}
		e.annoExpiry = now.Add(annoTTL)
	}
	announceURLs := e.cachedURLs
	opts := e.cachedOpts
	e.annoMu.Unlock()

	// Build Sources from cached announce URLs and fresh peer counts.
	// Only valid-URL schemes (udp://, http://, https://) are emitted — stremio-core
	// passes the url field through url::Url::parse, so "dht:" pseudo-URIs must not
	// appear here. Best-effort counts use totalPeers since anacrolix does not expose
	// per-tracker peer counts.
	started := now.UTC().Format(time.RFC3339)
	sources := make([]types.Source, 0, len(announceURLs))
	for _, u := range announceURLs {
		if !strings.HasPrefix(u, "udp://") && !strings.HasPrefix(u, "http://") &&
			!strings.HasPrefix(u, "https://") {
			continue // skip wss:// and any non-URL forms
		}
		numFound := totalPeers
		if numFound < 1 {
			numFound = 1 // non-zero best-effort
		}
		sources = append(sources, types.Source{
			LastStarted:  started,
			URL:          u,
			NumFound:     numFound,
			NumFoundUniq: numFound,
			NumRequests:  1,
		})
	}

	s := &types.Stats{
		InfoHash:          e.infoHash,
		Name:              e.t.Name(),
		Peers:             activePeers,
		Unchoked:          activePeers,
		Queued:            ts.HalfOpenPeers,
		Unique:            totalPeers,
		ConnectionTries:   0,
		SwarmPaused:       false,
		SwarmConnections:  activePeers,
		SwarmSize:         totalPeers,
		Selections:        nil,
		Wires:             []types.Wire{},
		Files:             files,
		Downloaded:        dl,
		Uploaded:          ul,
		DownloadSpeed:     dlSpeed,
		UploadSpeed:       ulSpeed,
		Sources:           sources,
		Opts:              opts,
		PeerSearchRunning: true,
	}

	// Per-file extras when a file index is requested.
	if idx >= 0 && e.hasInfo() {
		torFiles := e.t.Files()
		if idx < len(torFiles) {
			e.ensureDownloading(idx) // polling a file's stats also starts its download
			f := torFiles[idx]
			length := f.Length()
			name := f.DisplayPath()
			var progress float64
			if length > 0 {
				progress = float64(f.BytesCompleted()) / float64(length)
				if progress > 1 {
					progress = 1
				}
				if progress < 0 {
					progress = 0
				}
			}
			s.StreamLen = &length
			s.StreamName = &name
			s.StreamProgress = &progress
		}
	}

	return s
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// mergeTrackers adds tracker URLs from opts into t. It strips the "tracker:"
// prefix from Sources entries and ignores "dht:" entries.
func mergeTrackers(t *torrent.Torrent, opts types.AddOptions, allowWS bool) {
	var urls []string
	for _, tr := range opts.Trackers {
		tr = strings.TrimSpace(tr)
		if tr != "" {
			urls = append(urls, tr)
		}
	}
	for _, src := range opts.Sources {
		src = strings.TrimSpace(src)
		if strings.HasPrefix(src, "tracker:") {
			u := strings.TrimPrefix(src, "tracker:")
			if u != "" {
				urls = append(urls, u)
			}
		}
		// Ignore "dht:<infohash>" entries; anacrolix DHT finds peers on its own.
	}

	// Also extract trackers from the Torrent JSON field (stremio-web sends
	// {infoHash, announce, announce-list, files...}).
	urls = append(urls, trackersFromTorrentJSON(opts.Torrent)...)

	// Drop unannounceable schemes (ws/wss when WebTorrent is off) before adding;
	// anacrolix synchronously builds a tracker client and panics on bad schemes.
	urls = announceableTrackers(urls, allowWS)
	if len(urls) > 0 {
		// Each call to AddTrackers appends; duplicates are handled by anacrolix.
		t.AddTrackers([][]string{urls})
	}
}

// trackersFromTorrentJSON parses the stremio-web "torrent" JSON object and
// extracts any announce/announce-list tracker URLs. Returns nil on parse error.
func trackersFromTorrentJSON(raw json.RawMessage) []string {
	if raw == nil {
		return nil
	}
	var obj struct {
		Announce     string     `json:"announce"`
		AnnounceList [][]string `json:"announce-list"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	var urls []string
	if obj.Announce != "" {
		urls = append(urls, obj.Announce)
	}
	for _, tier := range obj.AnnounceList {
		urls = append(urls, tier...)
	}
	return urls
}

// applyBoundaryPriorities boosts piece priority on the first and last 8 pieces
// of the guessed video file so the container header (moov atom / codec-init box)
// and trailer (chapter index, etc.) are fetched immediately when a torrent
// becomes ready. This mirrors the reference server's MAX_CONTAINER_METADATA_WINDOW
// (16 MiB) behaviour: priming these pieces means ffprobe and the HLS transcoder
// can start without stalling on a seek to the end of the file.
//
// The function is designed to be called as a goroutine exactly once per torrent
// (guarded by e.onceMetaPriority). It blocks until metadata is available and
// then returns immediately after setting priorities.
func applyBoundaryPriorities(e *engine) {
	e.onceMetaPriority.Do(func() {
		// Block until info dict is fetched OR the torrent is dropped (Closed returns
		// a receive channel that is closed when the torrent is removed). Without the
		// Closed case this goroutine would leak for the lifetime of the process when
		// the torrent is evicted before metadata arrives.
		select {
		case <-e.t.GotInfo():
		case <-e.t.Closed():
			return
		}

		idx := e.GuessFileIdx()
		if idx < 0 {
			return
		}
		e.primeBoundary(idx)
	})
}

// primeBoundary raises piece priority on the first and last boundaryPieces pieces
// of file idx — the container header (MP4 moov atom / codec-init box) and trailer
// — so the player can parse the file and begin playback long before it is fully
// downloaded. It runs at most once per file (guarded by e.primed) and requires
// metadata to be available. Unlike the one-shot GuessFileIdx priming, this is also
// called from NewReader for the file the client actually requested — essential for
// multi-file torrents/packs where the played file is not the largest one, whose
// header would otherwise never be prioritized and playback would never start.
func (e *engine) primeBoundary(idx int) {
	if idx < 0 || !e.hasInfo() {
		return
	}
	e.mu.Lock()
	if e.primed == nil {
		e.primed = map[int]struct{}{}
	}
	if _, ok := e.primed[idx]; ok {
		e.mu.Unlock()
		return
	}
	e.primed[idx] = struct{}{}
	e.mu.Unlock()

	files := e.t.Files()
	if idx >= len(files) {
		return
	}
	f := files[idx]
	begin := f.BeginPieceIndex()
	end := f.EndPieceIndex() // exclusive
	const boundaryPieces = 8 // ~8 pieces × piece_length ≈ several MiB
	for i := begin; i < begin+boundaryPieces && i < end; i++ {
		e.t.Piece(i).SetPriority(torrent.PiecePriorityNow)
	}
	tailStart := end - boundaryPieces
	if tailStart < begin {
		tailStart = begin // file has fewer than 2×boundaryPieces pieces
	}
	for i := tailStart; i < end; i++ {
		e.t.Piece(i).SetPriority(torrent.PiecePriorityNow)
	}
	logging.For("engine").Debug("boundary-prioritized pieces", "info_hash", e.infoHash, "file_idx", idx, "begin", begin, "end", end, "head", min(boundaryPieces, end-begin), "tail", min(boundaryPieces, end-tailStart))
}

// min/max helpers for int and int64 (pre-Go 1.21 generics workaround; these
// are used locally and do not conflict with the builtins added in 1.21+).
func min[T int | int64 | float64](a, b T) T {
	if a < b {
		return a
	}
	return b
}

func max[T int | int64 | float64](a, b T) T {
	if a > b {
		return a
	}
	return b
}

// peerBudget derives the anacrolix per-torrent connection budget from a single
// knob. n<=0 uses the default 50. half-open = n/2, high-water = n*10 — the same
// ratios as the historical 50/25/500 defaults (peerBudget(0) == 50,25,500).
func peerBudget(n int) (established, halfOpen, highWater int) {
	if n <= 0 {
		n = 50
	}
	return n, n / 2, n * 10
}

// --------------------------------------------------------------------------
// Censorship-resistance / anonymity helpers
// --------------------------------------------------------------------------

// applyBTEncryption sets the MSE/RC4 header-obfuscation policy on cc.
// mode is compared case-insensitively after trimming whitespace.
//   - "require": only RC4-encrypted peer connections are accepted.
//   - "disable": plaintext only; no MSE handshake offered.
//   - anything else ("prefer" / ""): anacrolix defaults are left untouched
//     (MSE preferred, plaintext accepted — already the safest default).
func applyBTEncryption(cc *torrent.ClientConfig, mode string) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "require":
		cc.HeaderObfuscationPolicy = torrent.HeaderObfuscationPolicy{Preferred: true, RequirePreferred: true}
		cc.CryptoProvides = mse.CryptoMethodRC4
		cc.CryptoSelector = func(mse.CryptoMethod) mse.CryptoMethod { return mse.CryptoMethodRC4 }
		logging.For("engine").Info("bt encryption required", "mode", "require")
	case "disable":
		cc.HeaderObfuscationPolicy = torrent.HeaderObfuscationPolicy{Preferred: false, RequirePreferred: false}
		logging.For("engine").Info("bt encryption disabled", "mode", "disable")
		// "prefer" / "" — anacrolix defaults already correct; nothing to do
	}
}

// applyBTProxy routes tracker announces, HTTP webseeds, and metainfo fetches
// through proxyURL (socks5[h]://… or http[s]://…). When proxyURL is empty the
// function is a no-op and returns nil.
//
// Peer TCP/uTP connections are NOT routed through the proxy: anacrolix's peer
// dialer uses the listen-bound socket path (not HTTPDialContext/TrackerDialContext).
// Use encryption=require for DPI-evasion of peer traffic.
func applyBTProxy(cc *torrent.ClientConfig, proxyURL string) error {
	if proxyURL == "" {
		return nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return fmt.Errorf("parse proxy url: %w", err)
	}
	cc.HTTPProxy = http.ProxyURL(u)
	scheme := strings.ToLower(u.Scheme)
	if scheme == "socks5" || scheme == "socks5h" {
		d, err := proxy.FromURL(u, proxy.Direct)
		if err != nil {
			return fmt.Errorf("create socks5 dialer: %w", err)
		}
		if cd, ok := d.(proxy.ContextDialer); ok {
			cc.TrackerDialContext = cd.DialContext
			cc.HTTPDialContext = cd.DialContext
		}
	}
	// Log scheme://host only — never expose userinfo (credentials).
	logging.For("engine").Info("bittorrent proxy enabled", "url", u.Scheme+"://"+u.Host)
	return nil
}

// applyDHTBootstrap appends extra DHT bootstrap nodes to the default global
// list. nodes is a comma-separated list of host:port pairs; blank entries are
// silently skipped. When nodes is empty the function is a no-op.
func applyDHTBootstrap(cc *torrent.ClientConfig, nodes string) {
	if nodes == "" {
		return
	}
	var parsed []string
	for _, hp := range strings.Split(nodes, ",") {
		if hp = strings.TrimSpace(hp); hp != "" {
			parsed = append(parsed, hp)
		}
	}
	if len(parsed) == 0 {
		return
	}
	cc.DhtStartingNodes = func(network string) dht.StartingNodesGetter {
		return func() ([]dht.Addr, error) {
			addrs, _ := dht.GlobalBootstrapAddrs(network)
			for _, hp := range parsed {
				if ua, e := net.ResolveUDPAddr(network, hp); e == nil {
					addrs = append(addrs, dht.NewAddr(ua))
				}
			}
			return addrs, nil
		}
	}
	logging.For("engine").Info("extra dht bootstrap nodes", "count", len(parsed))
}
