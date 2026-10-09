// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

// Package api — NZB/Usenet streaming at /nzb/*.
//
// Routes:
//
//	POST /nzb/create
//	POST /nzb/create/{key}   body: {"servers":[{host,port,user,pass,ssl,connections}], "nzbUrl":"..."}
//	                         or ?lz=<lz-string-encoded-json>
//	                         → {"key":"<session-key>"}
//	GET  /nzb/create?lz=...  → creates the session like POST, then 307-redirects
//	                         to /nzb/stream/{key} (stremio-core's Stream::convert()
//	                         emits this GET form for NZB streams)
//	GET  /nzb/create         → 501 (no ?lz= payload)
//
//	GET  /nzb/stream?key={key}
//	GET  /nzb/stream/{key}/{file...}
//	                         → file served with Range/HEAD support; assembly is
//	                         progressive (a range is answered as soon as the
//	                         segments covering it are downloaded, no need to
//	                         wait for the whole file)
//
// Sessions expire after 1 hour of inactivity; extracted temp files are removed
// by a background janitor started lazily on first create.
package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	lzstring "github.com/daku10/go-lz-string"

	"github.com/M0Rf30/stremio-server-go/internal/netguard"
	"github.com/M0Rf30/stremio-server-go/internal/nzb"
)

// lz-string size guards — mirrors the limits used in archive.go and ftp.go.
const (
	nzbLzMaxEncoded = 1 << 20 // 1 MiB — max encoded ?lz= parameter length
	nzbLzMaxDecoded = 8 << 20 // 8 MiB — max decompressed JSON length
	// nzbMaxDownloadBytes caps the NZB XML fetched from the caller-supplied
	// nzbUrl (mirrors the cap httpGet used before this route switched to
	// archiveFetchGet for the guarded client).
	nzbMaxDownloadBytes = 16 << 20 // 16 MiB
)

// ---- session store ---------------------------------------------------------

// nzbFileState tracks the background assembly of a single file within a
// session. The assembly is shared: every request for the file reads from the
// same temp file while one set of parallel NNTP workers fills it, so a client
// disconnecting never stops it. The mutex only guards (re)starting; nothing
// blocks while holding it. A failed assembly is discarded and restarted by the
// next request.
type nzbFileState struct {
	mu      sync.Mutex
	asm     *nzb.Assembly // nil until the first request
	path    string        // temp file backing asm
	started time.Time     // stable Last-Modified while the file fills up
	restart int           // how many times a failed assembly was restarted
}

// nzbSession holds all state for one NZB streaming session.
type nzbSession struct {
	key        string
	cfg        nzb.ServerConfig
	files      []nzb.File
	tmpDir     string
	created    time.Time
	lastAccess time.Time
	refCount   int // in-flight requests; >0 blocks eviction (guarded by nzbSessionsMu)

	mu         sync.Mutex
	fileStates map[string]*nzbFileState // file Name → assembly state
}

var (
	nzbSessionsMu  sync.Mutex
	nzbSessions    = map[string]*nzbSession{}
	nzbJanitorOnce sync.Once
)

func nzbStartJanitor() {
	nzbJanitorOnce.Do(func() {
		go func() {
			// Reclaim assembled-file dirs leaked by a previous process.
			nzbSweepStale(os.TempDir())
			tick := time.NewTicker(10 * time.Minute)
			defer tick.Stop()
			for range tick.C {
				nzbEvictIdle()
				nzbSweepStale(os.TempDir())
			}
		}()
	})
}

// nzbTmpDirPrefix is the temp-dir prefix of NZB session dirs; only dirs with
// exactly this prefix are ever swept.
const nzbTmpDirPrefix = "stremio-nzb-"

// nzbSweepStale removes NZB session dirs under root that no live session owns
// and that have not been modified within an hour (leaked by a prior process).
func nzbSweepStale(root string) {
	live := map[string]struct{}{}
	nzbSessionsMu.Lock()
	for _, sess := range nzbSessions {
		live[sess.tmpDir] = struct{}{}
	}
	nzbSessionsMu.Unlock()
	sweepStaleTemp(root, live, time.Hour, func(e os.DirEntry) bool {
		return e.IsDir() && strings.HasPrefix(e.Name(), nzbTmpDirPrefix)
	})
}

// nzbEvictIdle removes sessions that have not been accessed in the last hour
// and have no in-flight requests, and deletes their temporary directories.
func nzbEvictIdle() {
	nzbSessionsMu.Lock()
	var evict []*nzbSession
	for key, sess := range nzbSessions {
		if sess.refCount > 0 {
			continue
		}
		if time.Since(sess.lastAccess) > time.Hour {
			evict = append(evict, sess)
			delete(nzbSessions, key)
		}
	}
	nzbSessionsMu.Unlock()

	for _, sess := range evict {
		sess.discard()
	}
}

// discard stops the session's background assemblies, waits (bounded) for
// their workers to release the temp files and NNTP connections, and deletes
// the session's temp directory.
func (sess *nzbSession) discard() {
	sess.mu.Lock()
	states := make([]*nzbFileState, 0, len(sess.fileStates))
	for _, fs := range sess.fileStates {
		states = append(states, fs)
	}
	sess.mu.Unlock()

	for _, fs := range states {
		fs.mu.Lock()
		asm := fs.asm
		fs.mu.Unlock()
		if asm == nil {
			continue
		}
		asm.Cancel()
		select {
		case <-asm.Done():
		case <-time.After(5 * time.Second):
		}
	}
	if sess.tmpDir != "" {
		_ = os.RemoveAll(sess.tmpDir)
	}
}

// ---- request body types ----------------------------------------------------

type nzbServerCfg struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	User        string `json:"user"`
	Pass        string `json:"pass"`
	SSL         bool   `json:"ssl"`
	Connections int    `json:"connections"`
}

type nzbCreateReq struct {
	Servers json.RawMessage `json:"servers"`
	NZBUrl  string          `json:"nzbUrl"`
	NZBUrls []string        `json:"nzbUrls"`
}

// ---- handler ---------------------------------------------------------------

// handleNZB dispatches all /nzb/* routes.
func (s *server) handleNZB(w http.ResponseWriter, r *http.Request, seg []string) {
	nzbStartJanitor()

	// seg[0] == "nzb"
	if len(seg) < 2 {
		http.NotFound(w, r)
		return
	}

	switch seg[1] {
	case "create":
		if r.Method == http.MethodGet && r.URL.Query().Get("lz") == "" {
			writeJSON(w, http.StatusNotImplemented, map[string]any{
				"error": "GET /nzb/create requires ?lz=; use POST with {servers, nzbUrl} or GET with ?lz=",
			})
			return
		}
		key := ""
		if len(seg) >= 3 {
			key = seg[2]
		}
		s.nzbCreate(w, r, key)
	case "stream":
		s.nzbStream(w, r, seg)
	default:
		http.NotFound(w, r)
	}
}

// nzbCreate handles POST /nzb/create[/{key}] and GET /nzb/create[/{key}]?lz=...
//   - POST → resolve, store session, respond {"key":…}.
//   - GET  → same, then 307-redirect to /nzb/stream/{key}.
//
// @Summary  Create an NZB/Usenet streaming session (POST, or GET with ?lz=)
// @Tags     NZB
// @Accept   json
// @Produce  json
// @Param    key   path   string  false  "caller-supplied session key"
// @Param    lz    query  string  false  "lz-string encoded JSON body"
// @Param    body  body   object  false  "{servers:[\"nntps://host\"], nzbUrl} (servers also accept {host,port,...} objects)"
// @Success  200  {object}  map[string]string  "session key"
// @Success  307  "redirect to /nzb/stream/{key} (GET form)"
// @Failure  400
// @Failure  501  "bare GET without ?lz="
// @Router   /nzb/create [get]
// @Router   /nzb/create [post]
// @Router   /nzb/create/{key} [get]
// @Router   /nzb/create/{key} [post]
func (s *server) nzbCreate(w http.ResponseWriter, r *http.Request, key string) {
	var req nzbCreateReq

	// Accept either ?lz=<compressed> or a plain JSON body.
	if lzParam := r.URL.Query().Get("lz"); lzParam != "" {
		if len(lzParam) > nzbLzMaxEncoded {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "lz: encoded payload too large"})
			return
		}
		jsonStr, err := lzstring.DecompressFromEncodedURIComponent(lzParam)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "lz: " + err.Error()})
			return
		}
		if len(jsonStr) > nzbLzMaxDecoded {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "lz: decompressed payload too large"})
			return
		}
		if err := json.Unmarshal([]byte(jsonStr), &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "json: " + err.Error()})
			return
		}
	} else {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "json: " + err.Error()})
			return
		}
	}

	nzbURL := req.NZBUrl
	if nzbURL == "" && len(req.NZBUrls) > 0 {
		nzbURL = req.NZBUrls[0]
	}
	if nzbURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "nzbUrl required"})
		return
	}

	servers, err := parseNzbServers(req.Servers)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if len(servers) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "servers list required"})
		return
	}

	// Fetch the NZB file from the provided URL. validateFetchHost is an
	// early 403; archiveFetchGet (archive.go, same package) performs the
	// actual fetch through archiveFetchClient, whose dialer re-validates
	// the resolved IP at connect time and whose redirect policy
	// re-validates every hop — api.go's getClient/httpGet must not be used
	// here since this route fetches a caller-supplied URL unauthenticated.
	if err := validateFetchHost(nzbURL); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	nzbData, err := archiveFetchGet(nzbURL, nzbMaxDownloadBytes)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	// Parse the NZB XML.
	files, err := nzb.Parse(nzbData)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "nzb: " + err.Error()})
		return
	}

	// Build server config from the first server entry. Control guards the
	// NNTP TCP/TLS dial itself: the client-supplied host/port in servers[]
	// previously bypassed netguard entirely (SEC-2), unlike nzbURL above.
	srv := servers[0]
	cfg := nzb.ServerConfig{
		Host:        srv.Host,
		Port:        srv.Port,
		User:        srv.User,
		Pass:        srv.Pass,
		SSL:         srv.SSL,
		Connections: srv.Connections,
		Control:     netguard.DialControl(!archiveAllowPrivateHosts()),
	}
	if cfg.Port == 0 {
		if cfg.SSL {
			cfg.Port = 563
		} else {
			cfg.Port = 119
		}
	}

	// Create an isolated temp directory for assembled file cache.
	tmpDir, err := os.MkdirTemp("", nzbTmpDirPrefix)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// Assign a random key when none is provided by the caller.
	if key == "" {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			_ = os.RemoveAll(tmpDir)
			return
		}
		key = fmt.Sprintf("%x", b)
	}

	sess := &nzbSession{
		key:        key,
		cfg:        cfg,
		files:      files,
		tmpDir:     tmpDir,
		created:    time.Now(),
		lastAccess: time.Now(),
		fileStates: map[string]*nzbFileState{},
	}

	nzbSessionsMu.Lock()
	// Evict any previous session under the same key.
	if old, ok := nzbSessions[key]; ok {
		go old.discard()
	}
	nzbSessions[key] = sess
	nzbSessionsMu.Unlock()

	if r.Method == http.MethodPost {
		writeJSON(w, http.StatusOK, map[string]any{"key": key})
		return
	}
	// GET → redirect straight to the stream URL; nzbResolveFile picks the file.
	http.Redirect(w, r, "/nzb/stream/"+url.PathEscape(key), http.StatusTemporaryRedirect)
}

// parseNzbServers accepts either the canonical stremio-core form — a JSON array
// of NNTP URL strings (nntp://user:pass@host:port, nntps:// for TLS) — or the
// legacy array of {host,port,user,pass,ssl,connections} objects.
func parseNzbServers(raw json.RawMessage) ([]nzbServerCfg, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("servers list required")
	}
	// Canonical form: array of NNTP URL strings.
	var urls []string
	if json.Unmarshal(raw, &urls) == nil && len(urls) > 0 {
		out := make([]nzbServerCfg, 0, len(urls))
		for _, u := range urls {
			cfg, err := nzbServerFromURL(u)
			if err != nil {
				return nil, err
			}
			out = append(out, cfg)
		}
		return out, nil
	}
	// Legacy form: array of server objects.
	var objs []nzbServerCfg
	if err := json.Unmarshal(raw, &objs); err != nil {
		return nil, fmt.Errorf("invalid servers: %w", err)
	}
	return objs, nil
}

// nzbServerFromURL parses an NNTP server URL into an nzbServerCfg. The scheme
// selects TLS (nntps/snews → SSL), userinfo carries credentials, and a missing
// port defaults later to 119 (plain) or 563 (TLS).
func nzbServerFromURL(raw string) (nzbServerCfg, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nzbServerCfg{}, fmt.Errorf("invalid nntp server url %q: %w", raw, err)
	}
	cfg := nzbServerCfg{
		Host: u.Hostname(),
		SSL:  u.Scheme == "nntps" || u.Scheme == "snews",
	}
	if cfg.Host == "" {
		return nzbServerCfg{}, fmt.Errorf("nntp server url %q has no host", raw)
	}
	if p := u.Port(); p != "" {
		cfg.Port, _ = strconv.Atoi(p)
	}
	if u.User != nil {
		cfg.User = u.User.Username()
		if pass, ok := u.User.Password(); ok {
			cfg.Pass = pass
		}
	}
	return cfg, nil
}

// nzbStream handles GET /nzb/stream?key={key} and /nzb/stream/{key}/{file...}.
// @Summary  Stream an assembled file from an NZB session
// @Tags     NZB
// @Produce  application/octet-stream
// @Param    key    query   string  false  "session key (query form)"
// @Param    file   path    string  false  "file name within the NZB"
// @Param    Range  header  string  false  "byte range (RFC 7233)"
// @Success  200
// @Success  206  {string}  string  "partial content"
// @Failure  404
// @Router   /nzb/stream [get]
// @Router   /nzb/stream/{key}/{file} [get]
func (s *server) nzbStream(w http.ResponseWriter, r *http.Request, seg []string) {
	// Resolve key and optional filename from the URL.
	var key, fileName string
	if len(seg) >= 3 {
		// /nzb/stream/{key}[/{file...}]
		key = seg[2]
		if len(seg) >= 4 {
			fileName = strings.Join(seg[3:], "/")
		}
	} else {
		// /nzb/stream?key=...
		key = r.URL.Query().Get("key")
	}

	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "session key required"})
		return
	}

	nzbSessionsMu.Lock()
	sess, ok := nzbSessions[key]
	if ok {
		sess.lastAccess = time.Now()
		sess.refCount++
	}
	nzbSessionsMu.Unlock()

	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
		return
	}
	defer func() {
		nzbSessionsMu.Lock()
		sess.refCount--
		nzbSessionsMu.Unlock()
	}()

	files := sess.files
	if len(files) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "NZB contains no files"})
		return
	}

	// Resolve which file to serve.
	target := nzbResolveFile(files, fileName)
	if target == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no suitable file found"})
		return
	}

	// Find target's index in sess.files so the temp filename is unique even when
	// two NZB entries share the same basename (prevents collision in tmpDir).
	fileIdx := 0
	for i := range sess.files {
		if sess.files[i].Name == target.Name {
			fileIdx = i
			break
		}
	}

	// Look up or initialise the per-file assembly state.
	sess.mu.Lock()
	fs, exists := sess.fileStates[target.Name]
	if !exists {
		fs = &nzbFileState{}
		sess.fileStates[target.Name] = fs
	}
	sess.mu.Unlock()

	// The first request starts the background assembly (parallel NNTP
	// connections filling a temp file); later ones share it. A previously
	// failed assembly is restarted from scratch.
	asm, assembledPath, modTime, err := fs.ensure(sess, target, fileIdx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	// The decoded size is known as soon as the first article's yEnc header has
	// been read — long before the file is complete — so the response can start
	// right away. r.Context() only bounds this request: if the client goes
	// away the shared assembly carries on for the next one.
	ctx := r.Context()
	size, err := asm.Size(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return // client disconnected
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "nzb assemble: " + err.Error()})
		return
	}

	// Open and serve the assembled file with full Range/HEAD support.
	f, err := os.Open(assembledPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer func() { _ = f.Close() }()

	w.Header().Set("Content-Type", mimeByName(target.Name))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("transferMode.dlna.org", "Streaming")
	w.Header().Set("contentFeatures.dlna.org",
		"DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000")

	if asm.Complete() {
		// Fully assembled: serve the plain file (keeps the sendfile fast path).
		http.ServeContent(w, r, target.Name, modTime, f)
		return
	}
	// Still downloading: each Read waits only for the segments it needs.
	http.ServeContent(w, r, target.Name, modTime, &nzbProgressiveFile{ctx: ctx, asm: asm, f: f, size: size})
}

// ensure returns the file's assembly, starting it when there is none or the
// previous one failed. The temp file is created before the assembly starts and
// closed once its workers have exited.
func (fs *nzbFileState) ensure(sess *nzbSession, target *nzb.File, fileIdx int) (*nzb.Assembly, string, time.Time, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	if fs.asm != nil && fs.asm.Err() == nil {
		return fs.asm, fs.path, fs.started, nil
	}
	prefix := strconv.Itoa(fileIdx)
	if fs.asm != nil {
		// Failed (or cancelled): drop the broken partial file. Readers still
		// attached to it keep their own open descriptor; the retry writes a
		// new, differently named file so stale workers can never touch it.
		fs.asm.Cancel()
		_ = os.Remove(fs.path)
		fs.restart++
		prefix += "r" + strconv.Itoa(fs.restart)
	}

	safeName := filepath.Base(target.Name)
	if safeName == "" || safeName == "." || safeName == "/" {
		safeName = "media.bin"
	}
	// Prefix with the file index to make the temp name unique per NZB entry.
	tmpPath := filepath.Join(sess.tmpDir, prefix+"-"+safeName)

	f, err := os.Create(tmpPath)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	// context.Background(), not the request's: the assembly outlives any one
	// client. It is stopped by nzbSession.discard (eviction / replacement).
	asm, err := nzb.NewSession(sess.cfg, sess.files).StartAssembly(context.Background(), target.Name, f)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return nil, "", time.Time{}, fmt.Errorf("nzb assemble: %w", err)
	}
	go func() {
		<-asm.Done()
		_ = f.Close()
	}()

	fs.asm, fs.path, fs.started = asm, tmpPath, time.Now()
	return fs.asm, fs.path, fs.started, nil
}

// nzbProgressiveFile is the io.ReadSeeker http.ServeContent reads while the
// file is still being assembled. Seek is pure arithmetic on the already-known
// size; Read blocks only until the segment holding its next byte is on disk
// (or the request's context ends) and then returns what is present.
type nzbProgressiveFile struct {
	ctx  context.Context
	asm  *nzb.Assembly
	f    *os.File
	size int64
	pos  int64
}

func (p *nzbProgressiveFile) Read(b []byte) (int, error) {
	if p.pos >= p.size {
		return 0, io.EOF
	}
	n := p.size - p.pos
	if int64(len(b)) < n {
		n = int64(len(b))
	}
	if n <= 0 {
		return 0, nil
	}
	// Wait for the first byte only, then read whatever contiguous run is on
	// disk (up to len(b)): the client gets data as it lands instead of the
	// handler stalling until a whole copy buffer's worth has been downloaded.
	avail, err := p.asm.WaitAvailable(p.ctx, p.pos, n)
	if err != nil {
		return 0, err
	}
	m, err := p.f.ReadAt(b[:avail], p.pos)
	p.pos += int64(m)
	switch {
	case int64(m) == avail:
		err = nil // ReadAt may report io.EOF together with a full read
	case errors.Is(err, io.EOF):
		err = io.ErrUnexpectedEOF // the assembly said these bytes were written
	}
	return m, err
}

func (p *nzbProgressiveFile) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = p.pos + offset
	case io.SeekEnd:
		abs = p.size + offset
	default:
		return 0, errors.New("nzb: invalid seek whence")
	}
	if abs < 0 {
		return 0, errors.New("nzb: negative seek position")
	}
	p.pos = abs
	return abs, nil
}

// nzbResolveFile selects which file to serve from the NZB.
//   - If fileName is given, match by name (basename comparison).
//   - Otherwise return the largest video-like file.
//   - Final fallback: first file.
func nzbResolveFile(files []nzb.File, fileName string) *nzb.File {
	if fileName != "" {
		base := filepath.Base(fileName)
		for i := range files {
			if files[i].Name == fileName || filepath.Base(files[i].Name) == base {
				return &files[i]
			}
		}
	}
	return nzbLargestVideo(files)
}

// nzbVideoExts lists recognised video file extensions for file selection.
var nzbVideoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".mov": true,
	".wmv": true, ".flv": true, ".webm": true, ".m4v": true,
	".mpg": true, ".mpeg": true, ".ts": true, ".m2ts": true,
}

// nzbLargestVideo returns the largest file whose extension is video-like,
// falling back to the first file when none matches.
func nzbLargestVideo(files []nzb.File) *nzb.File {
	var best *nzb.File
	for i := range files {
		ext := strings.ToLower(filepath.Ext(files[i].Name))
		if nzbVideoExts[ext] {
			if best == nil || files[i].Size > best.Size {
				best = &files[i]
			}
		}
	}
	if best == nil && len(files) > 0 {
		best = &files[0]
	}
	return best
}
