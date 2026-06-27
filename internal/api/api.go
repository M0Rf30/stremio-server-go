// Package api implements the enginefs-compatible HTTP surface that stremio-web
// expects from a Stremio streaming server. It depends only on the interfaces in
// internal/types, so the engine/settings/media implementations are pluggable.
//
// Niche routes fully wired: /list, /:ih/peers, /stream alias, /get-https (cert
// fetch from api.strem.io), /yt (yt-dlp shell-out), /casting (SSDP discovery),
// /local-addon (local-files Stremio addon), and HLS transcoding (/hlsv2 via the
// ffmpeg transcoder in internal/media). thumb.jpg is a cosmetic 404 stub.
package api

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/M0Rf30/stremio-server-go/internal/logging"
	"github.com/M0Rf30/stremio-server-go/internal/streamproxy"
	"github.com/M0Rf30/stremio-server-go/internal/types"
)

var infoHashRE = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)

// videoMimes augments mime.TypeByExtension for containers Go often misses.
var videoMimes = map[string]string{
	".mkv":  "video/x-matroska",
	".mp4":  "video/mp4",
	".m4v":  "video/x-m4v",
	".avi":  "video/x-msvideo",
	".mov":  "video/quicktime",
	".webm": "video/webm",
	".flv":  "video/x-flv",
	".wmv":  "video/x-ms-wmv",
	".mpg":  "video/mpeg",
	".mpeg": "video/mpeg",
	".ts":   "video/mp2t",
	".m2ts": "video/mp2t",
	".ogv":  "video/ogg",
}

type server struct {
	em         types.EngineManager
	ss         types.SettingsStore
	prober     types.MediaProber
	cfg        types.Config
	logReq     bool
	accessLog  *slog.Logger
	sp         *streamproxy.Handler
	certReload func() // hot-swap the live HTTPS cert after /get-https writes new files (wired by cmd)
}

// New returns the HTTP handler for the streaming server.
func New(em types.EngineManager, ss types.SettingsStore, prober types.MediaProber, cfg types.Config) http.Handler {
	localIMDBDisabled.Store(!cfg.LocalIMDB)
	return &server{
		em:        em,
		ss:        ss,
		prober:    prober,
		cfg:       cfg,
		logReq:    os.Getenv("STREMIO_HTTP_LOG") != "",
		accessLog: logging.For("http"),
		sp:        streamproxy.New(buildStreamProxyConfig(cfg)),
	}
}

// Shared, immutable CORS header values, assigned directly into the response
// header map (canonical keys) to avoid a per-request slice allocation and key
// canonicalization on every request.
var (
	corsAllowOrigin   = []string{"*"}
	corsExposeHeaders = []string{"Content-Range, Accept-Ranges, Content-Length, Content-Type"}
)

// ServeHTTP applies CORS, handles preflight, and dispatches to the router.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hdr := w.Header()
	hdr["Access-Control-Allow-Origin"] = corsAllowOrigin
	hdr["Access-Control-Expose-Headers"] = corsExposeHeaders
	if r.Method == http.MethodOptions {
		h := r.Header.Get("Access-Control-Request-Headers")
		if h == "" {
			h = "Range"
		}
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", h)
		w.Header().Set("Access-Control-Max-Age", "1728000")
		w.WriteHeader(http.StatusOK)
		return
	}
	if s.logReq {
		start := time.Now()
		rec := logging.NewResponseRecorder(w)
		s.route(rec, r)
		s.accessLog.Info("request",
			"method", r.Method,
			"uri", r.URL.RequestURI(),
			"status", rec.StatusOrOK(),
			"duration_ms", time.Since(start).Milliseconds(),
			"bytes", rec.Bytes,
			"remote", r.RemoteAddr,
		)
		return
	}
	s.route(w, r)
}

func (s *server) route(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(r.URL.Path, "/")
	if path == "" {
		s.handleLanding(w, r)
		return
	}
	seg := strings.Split(path, "/")

	// infoHash-scoped routes
	if infoHashRE.MatchString(seg[0]) {
		ih := strings.ToLower(seg[0])
		switch {
		case len(seg) == 1:
			http.NotFound(w, r)
		case seg[1] == "create":
			s.handleCreate(w, r, ih)
		case seg[1] == "remove":
			s.handleRemove(w, r, ih)
		case seg[1] == "stats.json":
			s.writeStats(w, ih, -1)
		case len(seg) >= 3 && seg[2] == "stats.json":
			idx, _ := strconv.Atoi(seg[1])
			s.writeStats(w, ih, idx)
		// /:infoHash/peers — peer list (best-effort via Stats.Wires)
		case seg[1] == "peers":
			s.handlePeers(w, r, ih)
		// /:infoHash/:fileIdx/subtitles.vtt — subtitle conversion for a torrent file
		case len(seg) >= 3 && seg[2] == "subtitles.vtt":
			idx, _ := strconv.Atoi(seg[1])
			s.handleStreamSubtitles(w, r, ih, idx)
		default:
			s.handleStream(w, r, ih, seg[1])
		}
		return
	}

	// top-level routes
	switch seg[0] {
	case "create":
		s.handleCreateBlob(w, r)
	case "removeAll":
		s.handleRemoveAll(w, r)
	case "stats.json":
		s.handleAllStats(w, r)
	case "network-info":
		s.handleNetworkInfo(w, r)
	case "device-info":
		s.handleDeviceInfo(w, r)
	case "settings":
		s.handleSettings(w, r)
	case "heartbeat":
		s.handleHeartbeat(w, r)
	case "probe":
		s.handleProbe(w, r)
	case "hlsv2":
		s.handleHLS(w, r, seg)
	case "tracks":
		s.handleTracks(w, r, seg)
	case "opensubHash":
		s.handleOpenSubHash(w, r)
	case "subtitlesTracks":
		s.handleSubtitlesTracks(w, r)
	case "hwaccel-profiler":
		s.handleHwaccelProfiler(w, r)
	case "proxy":
		if s.sp.Route(w, r, seg) {
			return
		}
		s.handleProxy(w, r, seg)
	case "generate_url":
		s.sp.HandleGenerateURL(w, r)
	case "base64":
		s.sp.HandleBase64(w, r, seg)
	// /list — active infohash array
	case "list":
		s.handleList(w, r)
	// /stream/:infoHash/:fileIdx — alias to /:infoHash/:fileIdx
	case "stream":
		if len(seg) >= 3 && infoHashRE.MatchString(seg[1]) {
			s.handleStream(w, r, strings.ToLower(seg[1]), seg[2])
		} else {
			http.NotFound(w, r)
		}
	case "get-https":
		s.handleGetHTTPS(w, r)
	case "casting":
		s.handleCasting(w, r, seg)
	case "yt":
		if len(seg) < 2 {
			http.NotFound(w, r)
			return
		}
		s.handleYT(w, r, seg[1])
	case "local-addon":
		s.handleLocalAddon(w, r, seg)
	case "bitmagnet":
		s.handleBitmagnet(w, r, seg)
	case "torznab":
		s.handleTorznab(w, r, seg)
	case "zip", "rar", "7zip", "tar", "tgz":
		s.handleArchive(w, r, seg, seg[0])
	case "nzb":
		s.handleNZB(w, r, seg)
	case "ftp":
		s.handleFTP(w, r, seg)
	case "thumb.jpg":
		http.NotFound(w, r) // no thumbnail service; cosmetic 404
	case "metrics":
		s.handleMetrics(w, r)
	default:
		if strings.HasPrefix(seg[0], "subtitles.") {
			s.handleSubtitles(w, r, strings.TrimPrefix(seg[0], "subtitles."))
			return
		}
		if strings.HasSuffix(seg[0], ".m3u8") || strings.HasSuffix(path, ".m3u8") {
			http.Error(w, "transcoding not implemented", http.StatusNotImplemented)
			return
		}
		http.NotFound(w, r)
	}
}

// ---- inline-route handlers -----------------------------------------------

// @Summary     Remove all active torrent engines
// @Tags        Engine
// @Produce     json
// @Success     200  {object}  map[string]interface{}
// @Router      /removeAll [get]
func (s *server) handleRemoveAll(w http.ResponseWriter, _ *http.Request) {
	s.em.RemoveAll()
	writeJSON(w, http.StatusOK, map[string]any{})
}

// @Summary     List available network interfaces
// @Tags        System
// @Produce     json
// @Success     200  {object}  map[string]interface{}
// @Router      /network-info [get]
func (s *server) handleNetworkInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"availableInterfaces": availableInterfaces()})
}

// @Summary     Report available hardware acceleration devices
// @Tags        System
// @Produce     json
// @Success     200  {object}  map[string]interface{}
// @Router      /device-info [get]
func (s *server) handleDeviceInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"availableHardwareAccelerations": detectHWAccels()})
}

// @Summary     Liveness probe
// @Tags        System
// @Produce     json
// @Success     200  {object}  map[string]interface{}
// @Router      /heartbeat [get]
func (s *server) handleHeartbeat(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// handleMetrics serves GET /metrics in Prometheus text exposition format (v0.0.4).
// No authentication and no CORS headers: the endpoint is intended for
// localhost-trust scraping (e.g. a Prometheus instance on the same host).
//
// @Summary  Prometheus-format runtime and app gauges
// @Tags     System
// @Produce  text/plain
// @Success  200  {string}  string  "Prometheus text exposition"
// @Router   /metrics [get]
func (s *server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	var b strings.Builder

	writeGauge := func(name, help string, value float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n", name, help)
		fmt.Fprintf(&b, "# TYPE %s gauge\n", name)
		fmt.Fprintf(&b, "%s %g\n", name, value)
	}

	// Runtime gauges
	writeGauge("stremio_goroutines",
		"Current number of goroutines.",
		float64(runtime.NumGoroutine()))
	writeGauge("stremio_memstats_alloc_bytes",
		"Bytes of allocated heap objects (runtime.MemStats.Alloc).",
		float64(ms.Alloc))
	writeGauge("stremio_memstats_sys_bytes",
		"Total bytes of memory obtained from the OS (runtime.MemStats.Sys).",
		float64(ms.Sys))

	// App gauges — structural assertions keep the api package interface-clean.
	torrents := 0
	if v, ok := s.em.(interface{ NumTorrents() int }); ok {
		torrents = v.NumTorrents()
	}
	writeGauge("stremio_torrents_active",
		"Number of active torrent engines.",
		float64(torrents))

	hlsSessions := 0
	if v, ok := s.prober.(interface{ HLSSessions() int }); ok {
		hlsSessions = v.HLSSessions()
	}
	writeGauge("stremio_hls_sessions",
		"Number of active HLS transcode sessions.",
		float64(hlsSessions))

	cacheEntries, cacheBytes := s.sp.CacheStats()
	writeGauge("stremio_proxy_cache_entries",
		"Number of entries in the stream-proxy segment cache.",
		float64(cacheEntries))
	writeGauge("stremio_proxy_cache_bytes",
		"Total bytes used by the stream-proxy segment cache.",
		float64(cacheBytes))

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, b.String())
}

// @Summary     List available hardware acceleration profiles
// @Tags        System
// @Produce     json
// @Success     200  {array}  string
// @Router      /hwaccel-profiler [get]
func (s *server) handleHwaccelProfiler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, detectHWAccels())
}

// @Summary     List active info hashes
// @Tags        Engine
// @Produce     json
// @Success     200  {array}  string
// @Router      /list [get]
func (s *server) handleList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.em.ListEngines())
}

// @Summary     Remove a torrent engine by info hash
// @Tags        Engine
// @Produce     json
// @Param       infoHash  path  string  true  "info hash"
// @Success     200  {object}  map[string]interface{}
// @Router      /{infoHash}/remove [get]
func (s *server) handleRemove(w http.ResponseWriter, _ *http.Request, ih string) {
	_ = s.em.RemoveEngine(ih)
	writeJSON(w, http.StatusOK, map[string]any{})
}

// ---- streaming ------------------------------------------------------------

// @Summary  Stream a torrent file by info hash and file index
// @Tags     Streaming
// @Produce  application/octet-stream
// @Param    infoHash  path    string  true   "40-hex info hash"
// @Param    fileIdx   path    int     true   "file index"
// @Param    Range     header  string  false  "byte range"
// @Success  206  {string}  string  "partial content"
// @Success  200
// @Failure  416
// @Router   /{infoHash}/{fileIdx} [get]
func (s *server) handleStream(w http.ResponseWriter, r *http.Request, ih, idxSeg string) {
	q := r.URL.Query()
	trackers := q["tr"]
	mustInc := compileMustInclude(q["f"])

	eng, err := s.em.EnsureEngine(ih, types.AddOptions{Trackers: trackers})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ctx, cancel := withTimeout(r, 90*time.Second)
	defer cancel()
	if err := eng.Ready(ctx); err != nil {
		http.Error(w, "timed out waiting for metadata: "+err.Error(), http.StatusGatewayTimeout)
		return
	}
	files := eng.Files()

	idx := resolveIndex(idxSeg, files, mustInc, eng)
	if idx < 0 || idx >= len(files) {
		http.Error(w, "file index not found", http.StatusNotFound)
		return
	}
	f := files[idx]

	// ?external=1 -> redirect to the named URL
	if q.Get("external") != "" {
		loc := "/" + ih + "/" + url.PathEscape(f.Name)
		if q.Get("download") != "" {
			loc += "?download=1"
		}
		http.Redirect(w, r, loc, http.StatusTemporaryRedirect)
		return
	}

	rc, length, err := eng.NewReader(idx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", mimeByName(f.Name))
	w.Header().Set("Cache-Control", "max-age=0, no-cache")
	w.Header().Set("transferMode.dlna.org", "Streaming")
	w.Header().Set("contentFeatures.dlna.org", "DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000")
	if q.Get("download") != "" {
		w.Header().Set("Content-Disposition", contentDisposition(f.Name))
	}
	if subs := q.Get("subtitles"); subs != "" {
		w.Header().Set("CaptionInfo.sec", subs)
	}

	bufp := streamBufPool.Get().(*[]byte)
	defer streamBufPool.Put(bufp)

	start, end, isRange, unsat := parseRange(r.Header.Get("Range"), length)
	if unsat {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", length))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if isRange {
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, length))
		if r.Method != http.MethodHead {
			if _, err := rc.Seek(start, io.SeekStart); err != nil {
				http.Error(w, "seek: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		w.WriteHeader(http.StatusPartialContent)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = io.CopyBuffer(w, io.LimitReader(rc, end-start+1), *bufp)
		return
	}

	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.CopyBuffer(w, rc, *bufp)
}

// resolveIndex picks the file index from the URL segment, a fileMustInclude
// match, an exact filename, or the guess heuristic.
func resolveIndex(seg string, files []types.FileInfo, mustInc []*regexp.Regexp, eng types.Engine) int {
	if len(mustInc) > 0 {
		for i, f := range files {
			for _, re := range mustInc {
				if re.MatchString(f.Name) {
					return i
				}
			}
		}
		return -1 // mustInc filters provided but no file matched → caller returns 404
	}
	if n, err := strconv.Atoi(seg); err == nil {
		if n == -1 {
			return eng.GuessFileIdx()
		}
		return n
	}
	// non-numeric: treat as a (url-encoded) filename
	name, _ := url.PathUnescape(seg)
	for i, f := range files {
		if f.Name == name {
			return i
		}
	}
	return eng.GuessFileIdx()
}

func compileMustInclude(vals []string) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, v := range vals {
		if v == "" {
			continue
		}
		if len(v) > 1 && v[0] == '/' {
			if i := strings.LastIndex(v, "/"); i > 0 {
				body, flags := v[1:i], v[i+1:]
				if strings.Contains(flags, "i") {
					body = "(?i)" + body
				}
				if re, err := regexp.Compile(body); err == nil {
					out = append(out, re)
					continue
				}
			}
		}
		if re, err := regexp.Compile(regexp.QuoteMeta(v)); err == nil {
			out = append(out, re)
		}
	}
	return out
}

// parseRange parses a single "bytes=start-end" range. Returns ok=true for a
// satisfiable range (→ 206), unsatisfiable=true when the Range header is
// syntactically valid but no byte is satisfiable (→ 416 per RFC 7233 §4.4),
// and both false when the header is absent or syntactically invalid (→ 200).
func parseRange(h string, length int64) (start, end int64, ok bool, unsatisfiable bool) {
	if h == "" || !strings.HasPrefix(h, "bytes=") {
		return 0, 0, false, false
	}
	spec := strings.TrimPrefix(h, "bytes=")
	if i := strings.IndexByte(spec, ','); i >= 0 {
		spec = spec[:i] // only the first range
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false, false
	}
	startS, endS := spec[:dash], spec[dash+1:]
	if startS == "" { // suffix range: bytes=-N
		n, err := strconv.ParseInt(endS, 10, 64)
		if err != nil {
			return 0, 0, false, false // syntactically invalid
		}
		if n <= 0 {
			return 0, 0, false, true // suffix-length 0 is unsatisfiable (RFC 7233 §4.4)
		}
		if n > length {
			n = length
		}
		if n == 0 { // length==0: suffix of empty file is unsatisfiable (RFC 7233 §4.4)
			return 0, 0, false, true
		}
		return length - n, length - 1, true, false
	}
	s, err := strconv.ParseInt(startS, 10, 64)
	if err != nil || s < 0 {
		return 0, 0, false, false // syntactically invalid
	}
	if s >= length {
		return 0, 0, false, true // first-byte-pos beyond EOF → 416
	}
	end = length - 1
	if endS != "" {
		if e, err := strconv.ParseInt(endS, 10, 64); err == nil {
			if e < s {
				return 0, 0, false, true // end < start → 416
			}
			end = e
		}
	}
	if end >= length {
		end = length - 1
	}
	return s, end, true, false
}

// ---- engine lifecycle -----------------------------------------------------

type createBody struct {
	Torrent         json.RawMessage `json:"torrent"`
	FileMustInclude []string        `json:"fileMustInclude"`
	GuessFileIdx    json.RawMessage `json:"guessFileIdx"`
	PeerSearch      *struct {
		Sources []string `json:"sources"`
	} `json:"peerSearch"`
}

// @Summary  Create or attach a torrent engine
// @Tags     Engine
// @Accept   json
// @Produce  json
// @Param    infoHash  path  string  true  "info hash"
// @Success  200  {object}  types.Stats
// @Router   /{infoHash}/create [get]
// @Router   /{infoHash}/create [post]
func (s *server) handleCreate(w http.ResponseWriter, r *http.Request, ih string) {
	var body createBody
	if r.Body != nil {
		dec := json.NewDecoder(io.LimitReader(r.Body, 3<<20))
		_ = dec.Decode(&body) // empty/invalid body is acceptable
	}

	opts := types.AddOptions{Torrent: body.Torrent}
	opts.Trackers = append(opts.Trackers, trackersFromSources(peerSearchSources(&body))...)
	opts.Trackers = append(opts.Trackers, announceFromTorrent(body.Torrent)...)

	eng, err := s.em.EnsureEngine(ih, opts)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ctx, cancel := withTimeout(r, 90*time.Second)
	defer cancel()
	if err := eng.Ready(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusGatewayTimeout)
		return
	}

	stats := eng.Stats(-1)
	if stats == nil {
		http.Error(w, "no stats", http.StatusInternalServerError)
		return
	}
	// guessedFileIdx
	if mi := compileMustInclude(body.FileMustInclude); len(mi) > 0 {
		files := eng.Files()
		for i, f := range files {
			for _, re := range mi {
				if re.MatchString(f.Name) {
					gi := i
					stats.GuessedFileIdx = &gi
				}
			}
		}
	}
	if stats.GuessedFileIdx == nil && truthy(body.GuessFileIdx) {
		gi := eng.GuessFileIdx()
		stats.GuessedFileIdx = &gi
	}
	writeJSON(w, http.StatusOK, stats)
}

// handleCreateBlob handles POST /create with {from:<url|path>} or {blob:<hex>}.
//
// @Summary  Create a torrent engine from URL or hex blob
// @Tags     Engine
// @Accept   json
// @Produce  json
// @Success  200  {object}  types.Stats
// @Router   /create [get]
// @Router   /create [post]
func (s *server) handleCreateBlob(w http.ResponseWriter, r *http.Request) {
	var body struct {
		From string `json:"from"`
		Blob string `json:"blob"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&body)
	}
	var raw []byte
	var err error
	switch {
	case body.Blob != "":
		raw, err = hexDecode(body.Blob)
	case strings.HasPrefix(body.From, "http://") || strings.HasPrefix(body.From, "https://"):
		raw, err = httpGet(body.From)
	case body.From != "":
		http.Error(w, "from: only http/https URLs are accepted", http.StatusBadRequest)
		return
	default:
		err = fmt.Errorf("missing from/blob")
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	mi, err := metainfo.Load(strings.NewReader(string(raw)))
	if err != nil {
		http.Error(w, "parse torrent: "+err.Error(), http.StatusInternalServerError)
		return
	}
	ih := mi.HashInfoBytes().HexString()
	eng, err := s.em.EnsureEngine(ih, types.AddOptions{MetaInfo: raw})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ctx, cancel := withTimeout(r, 90*time.Second)
	defer cancel()
	if err := eng.Ready(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusGatewayTimeout)
		return
	}
	writeJSON(w, http.StatusOK, eng.Stats(-1))
}

// @Summary  Get stats for a specific torrent engine file
// @Tags     Stats
// @Produce  json
// @Param    infoHash  path  string  true  "info hash"
// @Success  200  {object}  types.Stats
// @Router   /{infoHash}/stats.json [get]
// @Router   /{infoHash}/{fileIdx}/stats.json [get]
func (s *server) writeStats(w http.ResponseWriter, ih string, idx int) {
	eng, ok := s.em.GetEngine(ih)
	if !ok {
		writeJSON(w, http.StatusOK, nil)
		return
	}
	writeJSON(w, http.StatusOK, eng.Stats(idx))
}

// @Summary  Get stats for all active torrent engines
// @Tags     Stats
// @Produce  json
// @Success  200  {object}  map[string]types.Stats
// @Router   /stats.json [get]
func (s *server) handleAllStats(w http.ResponseWriter, r *http.Request) {
	all := s.em.AllStats()
	out := make(map[string]any, len(all)+1)
	for ih, st := range all {
		out[ih] = st
	}
	if r.URL.Query().Get("sys") == "1" {
		out["sys"] = map[string]any{"loadavg": loadavg(), "cpus": cpus()}
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePeers serves GET /:infoHash/peers — a JSON array of connected peers.
// Peer data is sourced from Stats.Wires (already available on the types.Engine
// interface) so we don't need to reach into the concrete engine internals.
//
// @Summary  List connected peers for a torrent engine
// @Tags     Stats
// @Produce  json
// @Param    infoHash  path  string  true  "info hash"
// @Success  200  {array}  object
// @Router   /{infoHash}/peers [get]
func (s *server) handlePeers(w http.ResponseWriter, r *http.Request, ih string) {
	eng, ok := s.em.GetEngine(ih)
	if !ok {
		writeJSON(w, http.StatusNotFound, []any{})
		return
	}
	st := eng.Stats(-1)
	if st == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	type peerInfo struct {
		Addr      string  `json:"addr"`
		AmInt     bool    `json:"amInterested"`
		IsSeeder  bool    `json:"isSeeder"`
		DownSpeed float64 `json:"downloadSpeed"`
		UpSpeed   float64 `json:"uploadSpeed"`
		Requests  int     `json:"requests"`
	}
	out := make([]peerInfo, 0, len(st.Wires))
	for _, wire := range st.Wires {
		out = append(out, peerInfo{
			Addr:      wire.Address,
			AmInt:     wire.AmInterested,
			IsSeeder:  wire.IsSeeder,
			DownSpeed: wire.DownSpeed,
			UpSpeed:   wire.UpSpeed,
			Requests:  wire.Requests,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleStreamSubtitles serves GET /:infoHash/:fileIdx/subtitles.vtt.
// It constructs a local stream URL for the torrent file and delegates subtitle
// conversion to the existing prober.WriteSubtitles path.
//
// @Summary  Stream subtitles as WebVTT for a torrent file
// @Tags     Media
// @Produce  text/vtt
// @Success  200  {string}  string  "WebVTT"
// @Router   /{infoHash}/{fileIdx}/subtitles.vtt [get]
func (s *server) handleStreamSubtitles(w http.ResponseWriter, r *http.Request, ih string, idx int) {
	// Build the local HTTP URL that the subtitle prober will pull from.
	streamURL := fmt.Sprintf("http://127.0.0.1:%d/%s/%d", s.cfg.HTTPPort, ih, idx)
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	// best-effort: WriteSubtitles may fail if the file isn't a subtitle stream
	_ = s.prober.WriteSubtitles(w, streamURL, "vtt", 0)
}

// ---- info / settings ------------------------------------------------------

// @Summary  Get or update server settings
// @Tags     Settings
// @Accept   json
// @Produce  json
// @Success  200  {object}  map[string]interface{}
// @Router   /settings [get]
// @Router   /settings [post]
func (s *server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var patch map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&patch)
		}
		if patch != nil {
			s.ss.Extend(patch)
			if err := s.ss.Save(); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
		return
	}
	ifaces := availableInterfaces()
	writeJSON(w, http.StatusOK, map[string]any{
		"options": s.ss.OptionsSchema(ifaces),
		"values":  s.ss.Values(),
		"baseUrl": s.baseURL(),
	})
}

// @Summary  Redirect to the Stremio web UI
// @Tags     System
// @Success  307
// @Router   / [get]
func (s *server) handleLanding(w http.ResponseWriter, r *http.Request) {
	scheme := "http://"
	if r.TLS != nil {
		scheme = "https://"
	}
	sep := "?"
	if strings.Contains(s.cfg.WebUI, "?") {
		sep = "&"
	}
	loc := s.cfg.WebUI + sep + "streamingServer=" + url.QueryEscape(scheme+r.Host)
	http.Redirect(w, r, loc, http.StatusTemporaryRedirect)
}

// ---- media helpers --------------------------------------------------------

// parseRational parses an ffprobe r_frame_rate string like "30000/1001" into a
// float64 frame rate.  Returns 0 for missing or malformed values.
func parseRational(s string) float64 {
	if s == "" || s == "0/0" {
		return 0
	}
	parts := strings.SplitN(s, "/", 2)
	if len(parts) == 1 {
		v, _ := strconv.ParseFloat(s, 64)
		return v
	}
	num, err1 := strconv.ParseFloat(parts[0], 64)
	den, err2 := strconv.ParseFloat(parts[1], 64)
	if err1 != nil || err2 != nil || den == 0 {
		return 0
	}
	return num / den
}

// reshapeProbeResult converts the raw ffprobe JSON map returned by Probe() into
// the enriched format the reference server returns.  The result includes:
//   - top-level duration (format.duration as float64)
//   - format.name (format_name)
//   - per-stream: index, track, codec, channels, width, height, fps, bitrate,
//     lang, default, profile — all sourced from the corresponding ffprobe fields.
//
// All new fields are ADDITIVE: existing callers (canPlayStream) still see
// track/codec/channels on every stream entry.
func reshapeProbeResult(raw map[string]interface{}) map[string]any {
	out := map[string]any{
		"format":   map[string]any{"name": ""},
		"duration": float64(0),
		"streams":  []any{},
	}
	if raw == nil {
		return out
	}
	if f, ok := raw["format"].(map[string]interface{}); ok {
		name, _ := f["format_name"].(string)
		var dur float64
		switch v := f["duration"].(type) {
		case float64:
			dur = v
		case string:
			dur, _ = strconv.ParseFloat(v, 64)
		}
		out["format"] = map[string]any{"name": name}
		out["duration"] = dur
	}
	if ss, ok := raw["streams"].([]interface{}); ok {
		streams := make([]any, 0, len(ss))
		for _, sv := range ss {
			sm, ok := sv.(map[string]interface{})
			if !ok {
				continue
			}
			// channels: only audio streams carry this; video/subtitle default to 0.
			ch, _ := sm["channels"].(float64)
			// fps: r_frame_rate is a fraction string ("25/1", "30000/1001", …).
			fps := parseRational(fmt.Sprintf("%v", sm["r_frame_rate"]))
			// bitrate: bit_rate is a decimal string in ffprobe JSON.
			var bitrate int64
			if brs, ok := sm["bit_rate"].(string); ok {
				bitrate, _ = strconv.ParseInt(brs, 10, 64)
			}
			// lang from tags sub-object.
			var lang string
			if tags, ok := sm["tags"].(map[string]interface{}); ok {
				lang, _ = tags["language"].(string)
			}
			// default from disposition sub-object (ffprobe stores it as a number).
			var isDefault bool
			if disp, ok := sm["disposition"].(map[string]interface{}); ok {
				if dv, ok := disp["default"].(float64); ok {
					isDefault = dv != 0
				}
			}
			profile, _ := sm["profile"].(string)
			width, _ := sm["width"].(float64)
			height, _ := sm["height"].(float64)
			idx, _ := sm["index"].(float64)

			entry := map[string]any{
				"index":    int(idx),
				"track":    sm["codec_type"],
				"codec":    sm["codec_name"],
				"channels": int(ch),
				"width":    int(width),
				"height":   int(height),
				"fps":      fps,
				"bitrate":  bitrate,
				"lang":     lang,
				"default":  isDefault,
				"profile":  profile,
			}
			streams = append(streams, entry)
		}
		out["streams"] = streams
	}
	return out
}

// handleProbe answers /probe?url=… by reshaping the raw ffprobe output into the
// enriched reference format (index/track/codec/channels/width/height/fps/
// bitrate/lang/default/profile per stream, plus top-level duration).
//
// @Summary  Probe a stream URL with ffprobe
// @Tags     Media
// @Produce  json
// @Param    url  query  string  true  "stream URL"
// @Success  200  {object}  map[string]interface{}
// @Router   /probe [get]
func (s *server) handleProbe(w http.ResponseWriter, r *http.Request) {
	res, err := s.prober.Probe(r.URL.Query().Get("url"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, res)
		return
	}
	raw, _ := res.(map[string]interface{})
	writeJSON(w, http.StatusOK, reshapeProbeResult(raw))
}

// handleHLSProbe answers /hlsv2/probe?mediaURL=… in the shape stremio-video's
// canPlayStream expects.  The response includes all enriched fields from the
// reference: index/track/codec/channels/width/height/fps/bitrate/lang/default/
// profile per stream, plus top-level duration and format.name.
//
// @Summary  Probe a media URL for HLS transcoding
// @Tags     HLS
// @Produce  json
// @Param    mediaURL  query  string  true  "media URL"
// @Success  200  {object}  map[string]interface{}
// @Router   /hlsv2/probe [get]
func (s *server) handleHLSProbe(w http.ResponseWriter, r *http.Request) {
	res, err := s.prober.Probe(r.URL.Query().Get("mediaURL"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	raw, _ := res.(map[string]interface{})
	writeJSON(w, http.StatusOK, reshapeProbeResult(raw))
}

// handleHLS dispatches /hlsv2/* : probe, the per-session master playlist, and
// the live media playlist + .ts segments produced by ffmpeg.
//
// @Summary  Serve HLS playlist or segment
// @Tags     HLS
// @Produce  application/vnd.apple.mpegurl
// @Param    id    path  string  true  "session id"
// @Param    file  path  string  true  "playlist or segment"
// @Success  200  {string}  string  "m3u8 / MPEG-TS / WebVTT"
// @Router   /hlsv2/{id}/{file} [get]
func (s *server) handleHLS(w http.ResponseWriter, r *http.Request, seg []string) {
	if len(seg) >= 2 && seg[1] == "probe" {
		s.handleHLSProbe(w, r)
		return
	}
	if len(seg) < 3 {
		http.NotFound(w, r)
		return
	}
	id, file := seg[1], seg[2]
	if file == "master.m3u8" {
		master, err := s.prober.StartHLS(id, r.URL.Query().Get("mediaURL"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write([]byte(master))
		return
	}
	path, ct, err := s.prober.HLSFile(r.Context(), id, file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, path)
}

// @Summary  List media tracks for a URL
// @Tags     Media
// @Produce  json
// @Param    url  path  string  true  "stream URL"
// @Success  200  {array}  object
// @Router   /tracks/{url} [get]
func (s *server) handleTracks(w http.ResponseWriter, r *http.Request, seg []string) {
	raw := ""
	if len(seg) > 1 {
		raw, _ = url.PathUnescape(strings.Join(seg[1:], "/"))
	}
	res, err := s.prober.Tracks(raw)
	if err != nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// @Summary  Compute OpenSubtitles hash for a video URL
// @Tags     Media
// @Produce  json
// @Param    videoUrl  query  string  true  "video URL"
// @Success  200  {object}  map[string]interface{}
// @Router   /opensubHash [get]
func (s *server) handleOpenSubHash(w http.ResponseWriter, r *http.Request) {
	res, err := s.prober.OpenSubHash(r.URL.Query().Get("videoUrl"))
	writeJSON(w, statusFor(err), map[string]any{"error": errStr(err), "result": res})
}

// @Summary  List subtitle tracks for a subtitle URL
// @Tags     Media
// @Produce  json
// @Param    subsUrl  query  string  true  "subtitle URL"
// @Success  200  {object}  map[string]interface{}
// @Router   /subtitlesTracks [get]
func (s *server) handleSubtitlesTracks(w http.ResponseWriter, r *http.Request) {
	res, err := s.prober.SubtitlesTracks(r.URL.Query().Get("subsUrl"))
	writeJSON(w, statusFor(err), map[string]any{"error": errStr(err), "result": res})
}

// @Summary  Fetch and convert subtitles
// @Tags     Media
// @Param    ext     path   string  true   "vtt|srt"
// @Param    from    query  string  true   "subtitle URL"
// @Param    offset  query  int     false  "ms offset"
// @Success  200  {string}  string
// @Router   /subtitles.{ext} [get]
func (s *server) handleSubtitles(w http.ResponseWriter, r *http.Request, ext string) {
	q := r.URL.Query()
	offset := 0
	if o := q.Get("offset"); o != "" {
		offset, _ = strconv.Atoi(o)
	}
	if ext == "vtt" {
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	if err := s.prober.WriteSubtitles(w, q.Get("from"), ext, offset); err != nil {
		// headers may already be sent; best effort
		return
	}
}

// ---- helpers --------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func withTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

func hexDecode(s string) ([]byte, error) {
	return hex.DecodeString(strings.TrimSpace(s))
}

func httpGet(u string) ([]byte, error) {
	resp, err := getClient.Get(u)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", u, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

func mimeByName(name string) string {
	ext := strings.ToLower(name)
	if i := strings.LastIndexByte(ext, '.'); i >= 0 {
		ext = ext[i:]
		if m, ok := videoMimes[ext]; ok {
			return m
		}
		if m := mime.TypeByExtension(ext); m != "" {
			return m
		}
	}
	return "application/octet-stream"
}

func peerSearchSources(b *createBody) []string {
	if b.PeerSearch != nil {
		return b.PeerSearch.Sources
	}
	return nil
}

func trackersFromSources(sources []string) []string {
	var out []string
	for _, src := range sources {
		if strings.HasPrefix(src, "tracker:") {
			out = append(out, strings.TrimPrefix(src, "tracker:"))
		}
	}
	return out
}

func announceFromTorrent(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var m struct {
		Announce     string     `json:"announce"`
		AnnounceList [][]string `json:"announce-list"`
		Sources      []string   `json:"sources"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	var out []string
	if m.Announce != "" {
		out = append(out, m.Announce)
	}
	for _, tier := range m.AnnounceList {
		out = append(out, tier...)
	}
	out = append(out, trackersFromSources(m.Sources)...)
	return out
}

func truthy(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "false" && s != "null" && s != "0" && s != `""`
}

// Interface enumeration (net.Interfaces + Addrs) is a syscall-heavy operation
// that dominated /settings and /network-info latency, both of which stremio-web
// polls. Cache the result briefly; interfaces rarely change within the TTL.
var (
	ifaceCacheMu sync.RWMutex
	ifaceCache   []string
	ifaceCacheAt time.Time
)

const ifaceCacheTTL = 30 * time.Second

// availableInterfaces returns the cached interface list, recomputing it at most
// once per ifaceCacheTTL. The returned slice is shared and must not be mutated.
func availableInterfaces() []string {
	ifaceCacheMu.RLock()
	if ifaceCache != nil && time.Since(ifaceCacheAt) < ifaceCacheTTL {
		v := ifaceCache
		ifaceCacheMu.RUnlock()
		return v
	}
	ifaceCacheMu.RUnlock()

	v := computeInterfaces()
	ifaceCacheMu.Lock()
	ifaceCache, ifaceCacheAt = v, time.Now()
	ifaceCacheMu.Unlock()
	return v
}

// availableInterfaces returns non-internal, non-link-local IPv4 + global IPv6
// addresses. This is the IPv6 fix: the original server returned IPv4 only.
func computeInterfaces() []string {
	var out []string
	seen := map[string]bool{}
	ifaces, err := net.Interfaces()
	if err != nil {
		return []string{}
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
				continue
			}
			s := ip.String()
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

func (s *server) baseURL() string {
	for _, a := range availableInterfaces() {
		if net.ParseIP(a) != nil && strings.Count(a, ":") == 0 { // first IPv4
			return fmt.Sprintf("http://%s:%d", a, s.cfg.HTTPPort)
		}
	}
	return fmt.Sprintf("http://127.0.0.1:%d", s.cfg.HTTPPort)
}

func loadavg() []float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return []float64{0, 0, 0}
	}
	f := strings.Fields(string(b))
	out := make([]float64, 0, 3)
	for i := 0; i < 3 && i < len(f); i++ {
		v, _ := strconv.ParseFloat(f[i], 64)
		out = append(out, v)
	}
	for len(out) < 3 {
		out = append(out, 0)
	}
	return out
}

func cpus() []any {
	n := runtime.NumCPU()
	out := make([]any, n)
	for i := range out {
		out[i] = map[string]any{}
	}
	return out
}

func statusFor(err error) int {
	if err != nil {
		return http.StatusInternalServerError
	}
	return http.StatusOK
}

func errStr(err error) any {
	if err != nil {
		return err.Error()
	}
	return nil
}

// proxyClient handles /proxy reverse-proxy requests (no timeout for streaming).
var proxyClient = &http.Client{
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 10 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	},
}

// streamBufPool holds reusable 256 KB buffers for streaming copies.
var streamBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 256<<10)
		return &buf
	},
}

// getClient is shared across all httpGet calls so TCP connections are reused.
var getClient = &http.Client{Timeout: 30 * time.Second}

// buildStreamProxyConfig converts types.Config proxy fields to a streamproxy.Config.
func buildStreamProxyConfig(cfg types.Config) streamproxy.Config {
	// Decode ProxySecret: try hex then base64url then base64std.
	var secret []byte
	if cfg.ProxySecret != "" {
		if b, err := hex.DecodeString(cfg.ProxySecret); err == nil {
			secret = b
		} else if b, err := base64.RawURLEncoding.DecodeString(cfg.ProxySecret); err == nil {
			secret = b
		} else if b, err := base64.StdEncoding.DecodeString(cfg.ProxySecret); err == nil {
			secret = b
		}
	}

	// Parse ProxyIPACL: comma-separated CIDR strings.
	var ipACL []*net.IPNet
	for _, cidr := range strings.Split(cfg.ProxyIPACL, ",") {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		if _, ipNet, err := net.ParseCIDR(cidr); err == nil {
			ipACL = append(ipACL, ipNet)
		}
	}

	return streamproxy.Config{
		Password:      cfg.ProxyPassword,
		Secret:        secret,
		IPACL:         ipACL,
		Prebuffer:     cfg.ProxyPrebuffer,
		SegCacheTTL:   time.Duration(cfg.ProxySegCacheTTL) * time.Second,
		PublicURL:     cfg.ProxyPublicURL,
		Client:        proxyClient,
		UpstreamProxy: cfg.ProxyUpstream,
	}
}

// handleProxy implements /proxy/<opts>/<path> used by stremio-video to fetch an
// external stream with injected request/response headers (proxyStreamsEnabled or
// per-stream proxyHeaders). opts is a URLSearchParams string: d=<origin>,
// repeated h=<reqHeader>, repeated r=<respHeader>.
//
// @Summary  Reverse-proxy an external stream URL
// @Tags     Proxy
// @Param    rest  path  string  true  "opts/path"
// @Success  200  {string}  string
// @Router   /proxy/{rest} [get]
func (s *server) handleProxy(w http.ResponseWriter, r *http.Request, seg []string) {
	if len(seg) < 2 {
		http.NotFound(w, r)
		return
	}
	if err := s.sp.Authorize(r); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	opts, err := url.ParseQuery(seg[1])
	if err != nil {
		http.Error(w, "bad proxy options", http.StatusBadRequest)
		return
	}
	origin := opts.Get("d")
	if origin == "" {
		http.Error(w, "missing proxy target", http.StatusBadRequest)
		return
	}
	target := strings.TrimRight(origin, "/") + "/" + strings.Join(seg[2:], "/")
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	if err := s.sp.ValidateDest(target); err != nil {
		http.Error(w, "forbidden target", http.StatusForbidden)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	for _, h := range opts["h"] { // injected request headers "key:value"
		if i := strings.IndexByte(h, ':'); i > 0 {
			req.Header.Set(strings.TrimSpace(h[:i]), strings.TrimSpace(h[i+1:]))
		}
	}
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := proxyClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	for _, h := range opts["r"] { // injected response headers; skip CORS headers the caller must not override
		if i := strings.IndexByte(h, ':'); i > 0 {
			k := http.CanonicalHeaderKey(strings.TrimSpace(h[:i]))
			if strings.HasPrefix(k, "Access-Control-") {
				continue
			}
			w.Header().Set(k, strings.TrimSpace(h[i+1:]))
		}
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		bufp := streamBufPool.Get().(*[]byte)
		defer streamBufPool.Put(bufp)
		_, _ = io.CopyBuffer(w, resp.Body, *bufp)
	}
}

var (
	hwAccelsOnce  sync.Once
	hwAccelsCache []any
)

// detectHWAccels reports available hardware acceleration profiles (VAAPI on the
// render node) for device-info / hwaccel-profiler. Cached after first call.
func detectHWAccels() []any {
	hwAccelsOnce.Do(func() {
		hwAccelsCache = []any{}
		if _, err := os.Stat("/dev/dri/renderD128"); err != nil {
			return
		}
		out, err := exec.Command("ffmpeg", "-hide_banner", "-hwaccels").Output()
		if err == nil && strings.Contains(string(out), "vaapi") {
			hwAccelsCache = []any{"vaapi"}
		}
	})
	return hwAccelsCache
}

// contentDisposition builds a Content-Disposition attachment header value with
// an ASCII-safe filename= fallback (RFC 2183) and an RFC 5987 filename*= field
// for proper handling of non-ASCII filenames by modern browsers.
func contentDisposition(name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r >= 0x20 && r <= 0x7E && r != '"' && r != '\\' {
			return r
		}
		return '_'
	}, name)
	return "attachment; filename=\"" + ascii + "\"; filename*=UTF-8''" + rfc5987Encode(name)
}

// rfc5987Encode percent-encodes a UTF-8 string for use as an RFC 5987 ext-value.
// attr-char octets (ALPHA / DIGIT / selected symbols) pass through unchanged;
// all other bytes are %XX encoded over their UTF-8 representation.
func rfc5987Encode(s string) string {
	const hx = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '!' || c == '#' || c == '$' || c == '&' || c == '+' || c == '-' ||
			c == '.' || c == '^' || c == '_' || c == '`' || c == '|' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hx[c>>4])
			b.WriteByte(hx[c&0xF])
		}
	}
	return b.String()
}
