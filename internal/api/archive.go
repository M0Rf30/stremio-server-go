// Package api — archive streaming for zip / rar / 7zip / tar / tgz
//
// Routes dispatched by handleArchive (seg[0] is the format extension):
//
//	GET|POST /{ext}/create             — create session, return {"key":…} or redirect
//	GET|POST /{ext}/create/{key}       — same, with caller-supplied key
//	GET      /{ext}/stream             — ?key=&file= redirect to full stream URL
//	GET      /{ext}/stream/{key}       — redirect to /{ext}/stream/{key}/{selectedFile}
//	GET      /{ext}/stream/{key}/{…}   — extract entry (once) and serve with Range support
//
// Create payload (JSON; may be gzip+base62 encoded in ?lz= query param):
//
//	Object  {"url":"…","from":"…","fileIdx":0,"fileMustInclude":"…"}
//	Array   [{"url":"…",…}]   — first element is used
//
// "url" and "from" are synonyms for the archive source. http/https sources are
// downloaded to a temp file; everything else is treated as a local path.
package api

import (
	"crypto/rand"
	"encoding/hex"
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

	"github.com/M0Rf30/stremio-server-go/internal/archive"
)

// ── session store ────────────────────────────────────────────────────────────

type archiveSession struct {
	mu           sync.Mutex
	key          string
	archivePath  string // path to archive file (downloaded temp or original local)
	isTempArch   bool   // true → archivePath is a temp file owned by this session
	ext          string // archive format: zip|rar|7zip|tar|tgz
	tmpDir       string // temp dir for extracted entries
	selectedFile string // default entry selected at create time
	created      time.Time
	lastAccess   time.Time
	extracted    map[string]string // entry name → extracted temp file path (cache)
}

var (
	archiveSessions    = map[string]*archiveSession{}
	archiveSessionsMu  sync.Mutex
	archiveJanitorOnce sync.Once
)

const archiveSessionTTL = time.Hour

// archiveStartJanitor starts a background goroutine that evicts idle sessions.
// It is started at most once, on the first incoming request.
func archiveStartJanitor() {
	archiveJanitorOnce.Do(func() {
		go func() {
			t := time.NewTicker(10 * time.Minute)
			defer t.Stop()
			for range t.C {
				archiveEvict()
			}
		}()
	})
}

func archiveEvict() {
	archiveSessionsMu.Lock()
	defer archiveSessionsMu.Unlock()
	now := time.Now()
	for k, sess := range archiveSessions {
		sess.mu.Lock()
		idle := now.Sub(sess.lastAccess)
		isTmp := sess.isTempArch
		archPath := sess.archivePath
		tmpDir := sess.tmpDir
		sess.mu.Unlock()
		if idle > archiveSessionTTL {
			_ = os.RemoveAll(tmpDir)
			if isTmp {
				_ = os.Remove(archPath)
			}
			delete(archiveSessions, k)
		}
	}
}

// ── create-payload parsing ────────────────────────────────────────────────────

type archivePayload struct {
	URL             string          `json:"url"`
	From            string          `json:"from"`
	FileIdx         json.RawMessage `json:"fileIdx"`
	FileMustInclude json.RawMessage `json:"fileMustInclude"`
}

// archiveParsePayload decodes the request payload. It first checks the ?lz=
// query parameter (lz-string compressed JSON), then falls back to the request
// body. Both array and object forms are accepted; the first array element is
// used when the payload is an array.
func archiveParsePayload(r *http.Request) (*archivePayload, error) {
	var raw []byte

	if lzParam := r.URL.Query().Get("lz"); lzParam != "" {
		decoded, err := lzstring.DecompressFromEncodedURIComponent(lzParam)
		if err != nil {
			return nil, fmt.Errorf("lz decode: %w", err)
		}
		raw = []byte(decoded)
	} else if r.Body != nil {
		var err error
		raw, err = io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			return nil, fmt.Errorf("read body: %w", err)
		}
	}

	if len(raw) == 0 {
		return nil, fmt.Errorf("empty payload: provide ?lz= query param or a JSON request body")
	}

	// Accept both [{…}] (array) and {…} (object).
	var arr []archivePayload
	if json.Unmarshal(raw, &arr) == nil && len(arr) > 0 {
		p := arr[0]
		return &p, nil
	}
	var obj archivePayload
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("parse payload JSON: %w", err)
	}
	return &obj, nil
}

// ── entry selection ──────────────────────────────────────────────────────────

// archiveVideoExts lists media container extensions that qualify as "video"
// files for the largest-video fallback selection heuristic.
var archiveVideoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".mov": true,
	".wmv": true, ".flv": true, ".webm": true, ".m4v": true,
	".mpg": true, ".mpeg": true, ".ts": true, ".m2ts": true,
}

// archiveSelectEntry picks a single file entry from entries using the
// following priority:
//  1. Explicit numeric index in payload.FileIdx.
//  2. First entry matching any fileMustInclude filter (case-insensitive substring).
//  3. Largest video-extension file; falls back to largest file overall.
func archiveSelectEntry(entries []archive.Entry, payload *archivePayload) (string, error) {
	// Collect non-directory entries.
	var files []archive.Entry
	for _, e := range entries {
		if !e.IsDir {
			files = append(files, e)
		}
	}
	if len(files) == 0 {
		return "", fmt.Errorf("archive contains no files")
	}

	// 1. Explicit index.
	if len(payload.FileIdx) > 0 && string(payload.FileIdx) != "null" {
		idx := -1
		var n int
		if json.Unmarshal(payload.FileIdx, &n) == nil {
			idx = n
		} else {
			var s string
			if json.Unmarshal(payload.FileIdx, &s) == nil {
				idx, _ = strconv.Atoi(s)
			}
		}
		if idx >= 0 && idx < len(files) {
			return files[idx].Name, nil
		}
	}

	// 2. fileMustInclude filter.
	if len(payload.FileMustInclude) > 0 && string(payload.FileMustInclude) != "null" {
		var filters []string
		var s string
		if json.Unmarshal(payload.FileMustInclude, &filters) != nil {
			if json.Unmarshal(payload.FileMustInclude, &s) == nil {
				filters = []string{s}
			}
		}
		lower := func(t string) string { return strings.ToLower(t) }
		for _, f := range files {
			for _, filt := range filters {
				if strings.Contains(lower(f.Name), lower(filt)) {
					return f.Name, nil
				}
			}
		}
	}

	// 3. Largest video file, or largest file overall as a final fallback.
	return archiveLargestVideo(files), nil
}

func archiveLargestVideo(files []archive.Entry) string {
	var best archive.Entry
	for _, f := range files {
		ext := strings.ToLower(filepath.Ext(f.Name))
		if archiveVideoExts[ext] && f.Size > best.Size {
			best = f
		}
	}
	if best.Name != "" {
		return best.Name
	}
	// Fallback: largest file regardless of extension.
	best = files[0]
	for _, f := range files[1:] {
		if f.Size > best.Size {
			best = f
		}
	}
	return best.Name
}

// ── helper: download / key / path-encoding ───────────────────────────────────

// archiveDownload fetches u and streams it to a new temp file.
// The caller owns the file and must remove it when done.
func archiveDownload(u string) (string, error) {
	resp, err := getClient.Get(u)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: status %d", u, resp.StatusCode)
	}
	f, err := os.CreateTemp("", "stremio-archive-dl-*")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	name := f.Name()
	_ = f.Close()
	return name, nil
}

// archiveNewKey returns a random 32-hex-char session key.
func archiveNewKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// archiveEncodePath URL-encodes each component of an archive entry path
// (which uses forward slashes as separators), preserving the slash structure.
func archiveEncodePath(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// ── extraction ───────────────────────────────────────────────────────────────

// archiveExtractEntry extracts entryName from the session's archive to a temp
// file under sess.tmpDir, returning the temp file path. Subsequent calls for
// the same entryName return the cached path without re-extraction. Concurrent
// calls for the same entry are handled safely: only one extraction occurs and
// duplicate work is discarded.
func archiveExtractEntry(sess *archiveSession, entryName string) (string, error) {
	// Fast path: already extracted.
	sess.mu.Lock()
	if p, ok := sess.extracted[entryName]; ok {
		sess.mu.Unlock()
		return p, nil
	}
	sess.mu.Unlock()

	r, err := archive.OpenFile(sess.archivePath, sess.ext)
	if err != nil {
		return "", fmt.Errorf("open archive: %w", err)
	}
	defer r.Close()

	rc, err := r.Open(entryName)
	if err != nil {
		return "", fmt.Errorf("open entry %q: %w", entryName, err)
	}

	ext := filepath.Ext(entryName)
	f, err := os.CreateTemp(sess.tmpDir, "entry-*"+ext)
	if err != nil {
		_ = rc.Close()
		return "", fmt.Errorf("create temp: %w", err)
	}
	_, copyErr := io.Copy(f, rc)
	_ = rc.Close()
	_ = f.Close()
	if copyErr != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("extract %q: %w", entryName, copyErr)
	}
	tmpPath := f.Name()

	// Store under lock; discard our copy if another goroutine finished first.
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if existing, ok := sess.extracted[entryName]; ok {
		_ = os.Remove(tmpPath)
		return existing, nil
	}
	sess.extracted[entryName] = tmpPath
	return tmpPath, nil
}

// ── handler entry point ──────────────────────────────────────────────────────

// handleArchive is the top-level dispatcher for /{ext}/… routes where ext is
// one of zip, rar, 7zip, tar, tgz. It is wired by route() in api.go.
func (s *server) handleArchive(w http.ResponseWriter, r *http.Request, seg []string, ext string) {
	archiveStartJanitor()

	if len(seg) < 2 {
		http.NotFound(w, r)
		return
	}

	switch seg[1] {
	case "create":
		key := ""
		if len(seg) >= 3 {
			key = seg[2]
		}
		s.archiveHandleCreate(w, r, seg, ext, key)
	case "stream":
		s.archiveHandleStream(w, r, seg, ext)
	default:
		http.NotFound(w, r)
	}
}

// archiveHandleCreate processes /{ext}/create and /{ext}/create/{key}.
//
//   - POST → resolve archive, select entry, store session, respond {"key":…}.
//   - GET  → same, then 307-redirect to the stream URL.
//
// @Summary  Create an archive streaming session (zip/rar/7zip/tar/tgz)
// @Tags     Archive
// @Accept   json
// @Produce  json
// @Param    archiveType  path   string  true   "archive format: zip, rar, 7zip, tar, or tgz"
// @Param    key          path   string  false  "caller-supplied session key"
// @Param    lz           query  string  false  "lz-string encoded JSON payload (url/fileIdx/fileMustInclude)"
// @Success  200  {object}  map[string]string  "session key"
// @Success  307  "redirect to the stream URL (GET)"
// @Failure  400
// @Failure  404
// @Router   /{archiveType}/create [get]
// @Router   /{archiveType}/create [post]
// @Router   /{archiveType}/create/{key} [get]
// @Router   /{archiveType}/create/{key} [post]
func (s *server) archiveHandleCreate(w http.ResponseWriter, r *http.Request, seg []string, ext, key string) {
	payload, err := archiveParsePayload(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Resolve source URL or local path.
	source := payload.URL
	if source == "" {
		source = payload.From
	}
	if source == "" {
		http.Error(w, "payload missing url or from field", http.StatusBadRequest)
		return
	}

	var archivePath string
	var isTempArch bool

	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		archivePath, err = archiveDownload(source)
		if err != nil {
			http.Error(w, "download failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		isTempArch = true
	} else {
		if _, statErr := os.Stat(source); statErr != nil {
			http.Error(w, "local path not found: "+statErr.Error(), http.StatusBadRequest)
			return
		}
		archivePath = source
	}

	// Open archive, list entries, select the target file.
	ar, err := archive.OpenFile(archivePath, ext)
	if err != nil {
		if isTempArch {
			_ = os.Remove(archivePath)
		}
		http.Error(w, "open archive: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	entries, listErr := ar.List()
	_ = ar.Close()
	if listErr != nil {
		if isTempArch {
			_ = os.Remove(archivePath)
		}
		http.Error(w, "list archive: "+listErr.Error(), http.StatusUnprocessableEntity)
		return
	}

	selected, err := archiveSelectEntry(entries, payload)
	if err != nil {
		if isTempArch {
			_ = os.Remove(archivePath)
		}
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}

	// Allocate session.
	if key == "" {
		key = archiveNewKey()
	}
	tmpDir, err := os.MkdirTemp("", "stremio-archive-")
	if err != nil {
		if isTempArch {
			_ = os.Remove(archivePath)
		}
		http.Error(w, "mkdirtemp: "+err.Error(), http.StatusInternalServerError)
		return
	}

	now := time.Now()
	sess := &archiveSession{
		key:          key,
		archivePath:  archivePath,
		isTempArch:   isTempArch,
		ext:          ext,
		tmpDir:       tmpDir,
		selectedFile: selected,
		created:      now,
		lastAccess:   now,
		extracted:    make(map[string]string),
	}
	archiveSessionsMu.Lock()
	archiveSessions[key] = sess
	archiveSessionsMu.Unlock()

	if r.Method == http.MethodPost {
		writeJSON(w, http.StatusOK, map[string]string{"key": key})
		return
	}
	// GET → redirect straight to the stream URL.
	http.Redirect(w, r,
		"/"+ext+"/stream/"+url.PathEscape(key)+"/"+archiveEncodePath(selected),
		http.StatusTemporaryRedirect)
}

// archiveHandleStream processes /{ext}/stream[/{key}[/{file…}]].
// @Summary  Stream a file from an archive session
// @Tags     Archive
// @Produce  application/octet-stream
// @Param    archiveType  path    string  true   "archive format: zip, rar, 7zip, tar, or tgz"
// @Param    key          path    string  true   "session key"
// @Param    file         path    string  false  "file path within the archive"
// @Param    Range        header  string  false  "byte range (RFC 7233)"
// @Success  200
// @Success  206  {string}  string  "partial content"
// @Success  307  "redirect to the canonical file URL"
// @Failure  404
// @Router   /{archiveType}/stream/{key}/{file} [get]
func (s *server) archiveHandleStream(w http.ResponseWriter, r *http.Request, seg []string, ext string) {
	// /{ext}/stream?key=…&file=…
	if len(seg) == 2 {
		key := r.URL.Query().Get("key")
		if key == "" {
			http.Error(w, "missing key query param", http.StatusBadRequest)
			return
		}
		archiveSessionsMu.Lock()
		sess, ok := archiveSessions[key]
		archiveSessionsMu.Unlock()
		if !ok {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		file := r.URL.Query().Get("file")
		if file == "" {
			sess.mu.Lock()
			file = sess.selectedFile
			sess.mu.Unlock()
		}
		http.Redirect(w, r,
			"/"+ext+"/stream/"+url.PathEscape(key)+"/"+archiveEncodePath(file),
			http.StatusTemporaryRedirect)
		return
	}

	key := seg[2]
	archiveSessionsMu.Lock()
	sess, ok := archiveSessions[key]
	archiveSessionsMu.Unlock()
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	// /{ext}/stream/{key} → redirect to selected file.
	if len(seg) == 3 {
		sess.mu.Lock()
		selected := sess.selectedFile
		sess.mu.Unlock()
		http.Redirect(w, r,
			"/"+ext+"/stream/"+url.PathEscape(key)+"/"+archiveEncodePath(selected),
			http.StatusTemporaryRedirect)
		return
	}

	// /{ext}/stream/{key}/{file…} → extract and serve.
	entryName := strings.Join(seg[3:], "/")

	filePath, err := archiveExtractEntry(sess, entryName)
	if err != nil {
		http.Error(w, "extract: "+err.Error(), http.StatusInternalServerError)
		return
	}

	sess.mu.Lock()
	sess.lastAccess = time.Now()
	sess.mu.Unlock()

	f, err := os.Open(filePath)
	if err != nil {
		http.Error(w, "open extracted file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", mimeByName(entryName))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("transferMode.dlna.org", "Streaming")
	w.Header().Set("contentFeatures.dlna.org",
		"DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000")

	http.ServeContent(w, r, entryName, time.Time{}, f)
}
