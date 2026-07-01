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
//	                         → assembled file served with Range/HEAD support
//
// Sessions expire after 1 hour of inactivity; extracted temp files are removed
// by a background janitor started lazily on first create.
package api

import (
	"crypto/rand"
	"encoding/json"
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

	"github.com/M0Rf30/stremio-server-go/internal/nzb"
)

// lz-string size guards — mirrors the limits used in archive.go and ftp.go.
const (
	nzbLzMaxEncoded = 1 << 20 // 1 MiB — max encoded ?lz= parameter length
	nzbLzMaxDecoded = 8 << 20 // 8 MiB — max decompressed JSON length
)

// ---- session store ---------------------------------------------------------

// nzbFileState tracks the assembly of a single file within a session.
// A mutex serialises concurrent callers. On success the result is cached
// permanently (done=true); on failure the lock is released so a subsequent
// request may retry, preserving the single-flight-on-success guarantee.
type nzbFileState struct {
	mu   sync.Mutex
	done bool // true once assembly has succeeded; path is valid forever
	path string
	err  error // last assembly error; meaningful only when !done
}

// nzbSession holds all state for one NZB streaming session.
type nzbSession struct {
	key        string
	cfg        nzb.ServerConfig
	files      []nzb.File
	tmpDir     string
	created    time.Time
	lastAccess time.Time

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
			tick := time.NewTicker(10 * time.Minute)
			defer tick.Stop()
			for range tick.C {
				nzbEvictIdle()
			}
		}()
	})
}

// nzbEvictIdle removes sessions that have not been accessed in the last hour
// and deletes their temporary directories.
func nzbEvictIdle() {
	nzbSessionsMu.Lock()
	var evict []*nzbSession
	for key, sess := range nzbSessions {
		if time.Since(sess.lastAccess) > time.Hour {
			evict = append(evict, sess)
			delete(nzbSessions, key)
		}
	}
	nzbSessionsMu.Unlock()

	for _, sess := range evict {
		if sess.tmpDir != "" {
			_ = os.RemoveAll(sess.tmpDir)
		}
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

	// Fetch the NZB file from the provided URL.
	nzbData, err := httpGet(nzbURL)
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

	// Build server config from the first server entry.
	srv := servers[0]
	cfg := nzb.ServerConfig{
		Host:        srv.Host,
		Port:        srv.Port,
		User:        srv.User,
		Pass:        srv.Pass,
		SSL:         srv.SSL,
		Connections: srv.Connections,
	}
	if cfg.Port == 0 {
		if cfg.SSL {
			cfg.Port = 563
		} else {
			cfg.Port = 119
		}
	}

	// Create an isolated temp directory for assembled file cache.
	tmpDir, err := os.MkdirTemp("", "stremio-nzb-")
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
	if old, ok := nzbSessions[key]; ok && old.tmpDir != "" {
		go func(d string) { _ = os.RemoveAll(d) }(old.tmpDir)
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
	}
	nzbSessionsMu.Unlock()

	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
		return
	}

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

	// Exactly one goroutine at a time performs assembly (others wait on fs.mu).
	// On success the path is cached permanently; on failure the mutex is released
	// so a later request can retry.
	assembledPath, assembleErr := func() (string, error) {
		fs.mu.Lock()
		defer fs.mu.Unlock()

		if fs.done {
			return fs.path, nil
		}

		safeName := filepath.Base(target.Name)
		if safeName == "" || safeName == "." || safeName == "/" {
			safeName = "media.bin"
		}
		// Prefix with the file index to make the temp name unique per NZB entry.
		tmpPath := filepath.Join(sess.tmpDir, fmt.Sprintf("%d-%s", fileIdx, safeName))

		f, err := os.Create(tmpPath)
		if err != nil {
			fs.err = err
			return "", err
		}

		nzbSess := nzb.NewSession(sess.cfg, sess.files)
		if err := nzbSess.AssembleFile(target.Name, f); err != nil {
			_ = f.Close()
			_ = os.Remove(tmpPath)
			fs.err = fmt.Errorf("nzb assemble: %w", err)
			return "", fs.err
		}
		_ = f.Close()
		fs.done = true
		fs.path = tmpPath
		fs.err = nil
		return tmpPath, nil
	}()

	if assembleErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": assembleErr.Error()})
		return
	}

	// Open and serve the assembled file with full Range/HEAD support.
	f, err := os.Open(assembledPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", mimeByName(target.Name))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("transferMode.dlna.org", "Streaming")
	w.Header().Set("contentFeatures.dlna.org",
		"DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000")

	http.ServeContent(w, r, target.Name, fi.ModTime(), f)
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
