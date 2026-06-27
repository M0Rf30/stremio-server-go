// Package api — NZB/Usenet streaming at /nzb/*.
//
// Routes:
//
//	POST /nzb/create
//	POST /nzb/create/{key}   body: {"servers":[{host,port,user,pass,ssl,connections}], "nzbUrl":"..."}
//	                         or ?lz=<lz-string-encoded-json>
//	                         → {"key":"<session-key>"}
//	GET  /nzb/create         → 501 (POST required)
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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	lzstring "github.com/daku10/go-lz-string"

	"github.com/M0Rf30/stremio-server-go/internal/nzb"
)

// ---- session store ---------------------------------------------------------

// nzbFileState tracks the assembly of a single file within a session.
// sync.Once guarantees exactly one download attempt per file even under
// concurrent requests.
type nzbFileState struct {
	once sync.Once
	path string
	err  error
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
	Servers []nzbServerCfg `json:"servers"`
	NZBUrl  string         `json:"nzbUrl"`
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
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusNotImplemented, map[string]any{
				"error": "GET /nzb/create is not supported; use POST with {servers, nzbUrl}",
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

// nzbCreate handles POST /nzb/create[/{key}].
func (s *server) nzbCreate(w http.ResponseWriter, r *http.Request, key string) {
	var req nzbCreateReq

	// Accept either ?lz=<compressed> or a plain JSON body.
	if lzParam := r.URL.Query().Get("lz"); lzParam != "" {
		jsonStr, err := lzstring.DecompressFromEncodedURIComponent(lzParam)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "lz: " + err.Error()})
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

	if len(req.Servers) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "servers list required"})
		return
	}
	if req.NZBUrl == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "nzbUrl required"})
		return
	}

	// Fetch the NZB file from the provided URL.
	nzbData, err := httpGet(req.NZBUrl)
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
	srv := req.Servers[0]
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

	writeJSON(w, http.StatusOK, map[string]any{"key": key})
}

// nzbStream handles GET /nzb/stream?key={key} and /nzb/stream/{key}/{file...}.
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

	// Look up or initialise the per-file assembly state.
	sess.mu.Lock()
	fs, exists := sess.fileStates[target.Name]
	if !exists {
		fs = &nzbFileState{}
		sess.fileStates[target.Name] = fs
	}
	sess.mu.Unlock()

	// Exactly one goroutine performs the assembly; others block until it finishes.
	fs.once.Do(func() {
		safeName := filepath.Base(target.Name)
		if safeName == "" || safeName == "." || safeName == "/" {
			safeName = "media.bin"
		}
		tmpPath := filepath.Join(sess.tmpDir, safeName)

		f, err := os.Create(tmpPath)
		if err != nil {
			fs.err = err
			return
		}

		nzbSess := nzb.NewSession(sess.cfg, sess.files)
		if err := nzbSess.AssembleFile(target.Name, f); err != nil {
			f.Close()
			_ = os.Remove(tmpPath)
			fs.err = fmt.Errorf("nzb assemble: %w", err)
			return
		}
		f.Close()
		fs.path = tmpPath
	})

	if fs.err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": fs.err.Error()})
		return
	}

	// Open and serve the assembled file with full Range/HEAD support.
	f, err := os.Open(fs.path)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer f.Close()

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
