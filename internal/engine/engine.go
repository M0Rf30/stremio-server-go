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
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/time/rate"

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
	lastAccess       time.Time        // updated on NewReader/Stats; used by the janitor for LRU eviction
	onceMetaPriority sync.Once        // ensures boundary-piece prioritization runs exactly once

	// Announce-URL cache: refreshed at most once per annoTTL to avoid acquiring
	// the anacrolix Metainfo()/DistinctValues() locks on every Stats() call.
	annoMu     sync.Mutex
	annoExpiry time.Time
	cachedURLs []string
	cachedOpts map[string]any // full Opts map; rebuilt when cachedURLs are refreshed
}

// ensureDownloading marks file idx for full background download (idempotent), so
// the torrent keeps fetching it even when no reader is actively pulling — this is
// what makes the download progress/stats climb like the official server.
func (e *engine) ensureDownloading(idx int) {
	if idx < 0 {
		return
	}
	e.mu.Lock()
	if e.selected == nil {
		e.selected = map[int]struct{}{}
	}
	if _, ok := e.selected[idx]; ok {
		e.mu.Unlock()
		return
	}
	e.selected[idx] = struct{}{}
	e.mu.Unlock()
	files := e.t.Files()
	if idx < len(files) {
		files[idx].Download()
	}
}

// manager owns the anacrolix Client and the live engine map, satisfying types.EngineManager.
type manager struct {
	client      *torrent.Client
	cfg         types.Config
	downLimiter *rate.Limiter // shared pointer in cc.DownloadRateLimiter; updated by SetLimitFn
	upLimiter   *rate.Limiter // shared pointer in cc.UploadRateLimiter; updated by SetLimitFn

	mu        sync.RWMutex
	engines   map[string]*engine // keyed by lower-cased infoHash
	done      chan struct{}      // closed by Close() to stop background goroutines
	closeOnce sync.Once          // ensures done is closed exactly once (idempotent Close)
	limitOnce sync.Once          // ensures SetLimitFn goroutine is started at most once
}

// Compile-time interface satisfaction checks.
var _ types.EngineManager = (*manager)(nil)
var _ types.Engine = (*engine)(nil)

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
	cc.DisablePEX = false        // BEP11 peer exchange
	cc.DisableWebtorrent = false // WebRTC/WebTorrent peers (needs wss trackers)
	cc.DisableWebseeds = false   // BEP19 HTTP webseeds
	cc.Seed = true               // keep uploading while/after streaming (swarm health; uses IPv6 inbound)
	cc.AcceptPeerConnections = true
	cc.DisableAcceptRateLimiting = false // rate-limit inbound accepts to bound connection-handling CPU

	// Connection budgets tuned for streaming: enough peers for full throughput
	// without excessive MSE/RC4 handshakes and dial churn, which are the dominant
	// engine CPU cost on large, fast swarms (4K). These match anacrolix defaults
	// (50/25) with a trimmed peer high-water; raise them only if throughput-bound.
	cc.EstablishedConnsPerTorrent = 50
	cc.HalfOpenConnsPerTorrent = 25
	cc.TorrentPeersHighWater = 500

	// Honor the documented contract: 0 = OS-assigned (random) port. anacrolix's
	// NewDefaultClientConfig hardcodes 42069, so we must set it unconditionally
	// or two instances (and the test suite) collide on that port.
	cc.ListenPort = cfg.ListenPort

	// File storage partitioned by info-hash so torrents don't collide.
	cc.DefaultStorage = storage.NewFileByInfoHash(cfg.CacheRoot)

	// Bandwidth rate limiters — start unlimited; SetLimitFn() adjusts them live.
	// We keep the *rate.Limiter pointers so we can call SetLimit/SetBurst without
	// changing the pointer stored in cc (anacrolix does not allow pointer replacement
	// after client creation). Burst for download: ≥1 MiB (anacrolix default floor);
	// for upload: ≥512 KiB (largest peer-request chunk size we might serve).
	downLimiter := rate.NewLimiter(rate.Inf, 1<<22) // 4 MiB burst, unlimited
	upLimiter := rate.NewLimiter(rate.Inf, 1<<22)   // 4 MiB burst, unlimited
	cc.DownloadRateLimiter = downLimiter
	cc.UploadRateLimiter = upLimiter

	client, err := torrent.NewClient(cc)
	if err != nil {
		return nil, fmt.Errorf("engine: create torrent client: %w", err)
	}

	// Create the done channel first so initTrackers can stop its 24h goroutine
	// when Close() is called.
	done := make(chan struct{})

	// Load curated trackers (cached) and refresh + rank from upstream in the background.
	initTrackers(cfg.CacheRoot, cfg.TrackersMax, done)

	return &manager{
		client:      client,
		cfg:         cfg,
		downLimiter: downLimiter,
		upLimiter:   upLimiter,
		engines:     make(map[string]*engine),
		done:        done,
	}, nil
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
		mergeTrackers(e.t, opts)
		return e, nil
	}
	m.mu.RUnlock()

	// Slow path: acquire write lock, re-check (double-check locking), then add.
	m.mu.Lock()
	defer m.mu.Unlock()

	if e, ok := m.engines[ih]; ok {
		mergeTrackers(e.t, opts)
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
		t, err = m.client.AddTorrent(mi)
	} else {
		// Only the info-hash is known; start with a plain magnet URI.
		magnet := "magnet:?xt=urn:btih:" + ih
		t, err = m.client.AddMagnet(magnet)
	}
	if err != nil {
		return nil, fmt.Errorf("engine: add torrent %s: %w", ih, err)
	}

	mergeTrackers(t, opts)
	// Inject a baseline public-tracker list (like the official server) so bare /
	// trackerless magnets still find peers instead of relying on DHT alone.
	t.AddTrackers([][]string{getTrackers()})

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
		e          *engine
		size       int64
		lastAccess time.Time
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
		e.mu.Unlock()
		sz := dirSizes[e.infoHash]
		entries = append(entries, entry{e: e, size: sz, lastAccess: la})
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
		if now.Sub(ent.lastAccess) < graceWindow {
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
		ent.e.t.Drop()
		delete(m.engines, ih)
		m.mu.Unlock()
		// Remove cached data from disk outside the write lock.
		_ = os.RemoveAll(ent.e.path)
		total -= ent.size
		log.Printf("engine: evicted %s (freed %d B; budget %d B)", ih, ent.size, budget)
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

// NewReader returns a streaming, seek-capable reader for the file at idx.
// Readahead is scaled to ~2 s of recent download throughput, clamped to
// [16 MiB, 64 MiB], so slow connections stay at the 16 MiB floor while fast
// ones (≥32 MB/s) reach the 64 MiB ceiling for smoother high-bitrate streams.
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
	e.ensureDownloading(idx) // keep fetching the whole file, not just the read window
	r := f.NewReader()

	// Compute readahead = clamp(2 s × downloadRate, 16 MiB, 64 MiB).
	// When cachedSpeed is 0 (no measurement yet) we use the 16 MiB floor.
	const minReadahead int64 = 16 << 20 // 16 MiB
	const maxReadahead int64 = 64 << 20 // 64 MiB
	readahead := minReadahead
	if cachedSpeed > 0 {
		scaled := int64(2 * cachedSpeed)
		if scaled < minReadahead {
			scaled = minReadahead
		}
		if scaled > maxReadahead {
			scaled = maxReadahead
		}
		readahead = scaled
	}
	r.SetReadahead(readahead)
	return r, f.Length(), nil
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
		e.cachedOpts = map[string]any{
			"connections":      nil,
			"dht":              true,
			"growler":          map[string]any{"flood": 0, "pulse": nil},
			"handshakeTimeout": nil,
			"path":             e.path,
			"peerSearch": map[string]any{
				"min":     40,
				"max":     200,
				"sources": peerSrcs,
			},
			"swarmCap": map[string]any{"maxSpeed": nil, "minPeers": nil},
			"timeout":  nil,
			"tracker":  true,
			"virtual":  false,
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
func mergeTrackers(t *torrent.Torrent, opts types.AddOptions) {
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
		files := e.t.Files()
		if idx >= len(files) {
			return
		}
		f := files[idx]
		begin := f.BeginPieceIndex()
		end := f.EndPieceIndex() // exclusive

		const boundaryPieces = 8 // ~8 pieces × piece_length ≈ several MiB

		// Raise priority on the first boundary_pieces pieces (moov header).
		for i := begin; i < begin+boundaryPieces && i < end; i++ {
			e.t.Piece(i).SetPriority(torrent.PiecePriorityNow)
		}
		// Raise priority on the last boundary_pieces pieces (chapter/moov tail).
		tailStart := end - boundaryPieces
		if tailStart < begin {
			tailStart = begin // file has fewer than 2×boundaryPieces pieces
		}
		for i := tailStart; i < end; i++ {
			e.t.Piece(i).SetPriority(torrent.PiecePriorityNow)
		}

		log.Printf("engine: boundary-prioritized %d+%d pieces for %s (file idx %d, pieces [%d,%d))",
			min(boundaryPieces, end-begin), min(boundaryPieces, end-tailStart),
			e.infoHash, idx, begin, end)
	})
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
