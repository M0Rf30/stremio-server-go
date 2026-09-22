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
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	lzstring "github.com/daku10/go-lz-string"

	"github.com/M0Rf30/stremio-server-go/internal/archive"
	"github.com/M0Rf30/stremio-server-go/internal/netguard"
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
	refCount     int               // in-flight requests; >0 blocks eviction (guarded by mu)
	extracted    map[string]string // entry name → extracted temp file path (cache)
}

var (
	archiveSessions    = map[string]*archiveSession{}
	archiveSessionsMu  sync.Mutex
	archiveJanitorOnce sync.Once
)

const archiveSessionTTL = time.Hour

// archiveMaxDownloadBytes is the maximum number of bytes that archiveDownload
// will stream from a remote URL to a local temp file. Archives larger than
// this limit are rejected to prevent unbounded disk consumption.
// 4 GiB covers the largest single-file archives seen in practice.
const archiveMaxDownloadBytes int64 = 4 << 30 // 4 GiB

// archiveMaxLZEncoded is the maximum accepted byte length of a raw ?lz= query
// value before decompression. Prevents memory blowup before decode begins.
const archiveMaxLZEncoded = 1 << 20 // 1 MiB

// archiveMaxLZDecoded is the maximum byte length of the JSON string produced
// by lz-string decompression. Guards against decompression bombs.
const archiveMaxLZDecoded = 8 << 20 // 8 MiB

// archiveMaxEntryBytes is the maximum declared (uncompressed) size accepted
// for a single archive entry during extraction. Entries claiming more than
// this are rejected before any data is copied, preventing integer overflow
// (declaredSize+1 wraps negative for values near math.MaxInt64) and unbounded
// disk writes from malicious ZIP64/tar archives. 64 GiB is far beyond any
// legitimate video entry seen in practice.
const archiveMaxEntryBytes int64 = 64 << 30 // 64 GiB

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
	type evictVictim struct {
		tmpDir   string
		archPath string
		isTmp    bool
	}
	now := time.Now()
	var victims []evictVictim

	archiveSessionsMu.Lock()
	for k, sess := range archiveSessions {
		sess.mu.Lock()
		idle := now.Sub(sess.lastAccess)
		inUse := sess.refCount > 0
		isTmp := sess.isTempArch
		archPath := sess.archivePath
		tmpDir := sess.tmpDir
		sess.mu.Unlock()
		if !inUse && idle > archiveSessionTTL {
			victims = append(victims, evictVictim{tmpDir: tmpDir, archPath: archPath, isTmp: isTmp})
			delete(archiveSessions, k)
		}
	}
	archiveSessionsMu.Unlock()

	// Perform disk I/O outside the lock so concurrent archive handlers are not
	// blocked while RemoveAll/Remove walk the filesystem.
	for _, v := range victims {
		_ = os.RemoveAll(v.tmpDir)
		if v.isTmp {
			_ = os.Remove(v.archPath)
		}
	}
}

// ── create-payload parsing ────────────────────────────────────────────────────

type archivePayload struct {
	URL             string          `json:"url"`
	From            string          `json:"from"`
	URLs            json.RawMessage `json:"urls"`
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
		if len(lzParam) > archiveMaxLZEncoded {
			return nil, fmt.Errorf("lz param too large (%d bytes; limit %d)", len(lzParam), archiveMaxLZEncoded)
		}
		decoded, err := lzstring.DecompressFromEncodedURIComponent(lzParam)
		if err != nil {
			return nil, fmt.Errorf("lz decode: %w", err)
		}
		if len(decoded) > archiveMaxLZDecoded {
			return nil, fmt.Errorf("lz decoded payload too large (%d bytes; limit %d)", len(decoded), archiveMaxLZDecoded)
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

// source returns the archive source URL/path. It accepts the legacy `url`/`from`
// string fields and the canonical `urls` array used by stremio-core, whose
// entries are `[url, bytes?]` tuples (it also tolerates `["url"]` strings or
// `{"url":…}` objects). The first entry is used.
func (p *archivePayload) source() string {
	if p.URL != "" {
		return p.URL
	}
	if p.From != "" {
		return p.From
	}
	if len(p.URLs) == 0 {
		return ""
	}
	var entries []json.RawMessage
	if json.Unmarshal(p.URLs, &entries) != nil || len(entries) == 0 {
		return ""
	}
	return archiveURLFromEntry(entries[0])
}

// archiveURLFromEntry extracts a URL string from one `urls` entry, which may be
// a bare string, a `[url, bytes?]` tuple, or a `{"url":…}` object.
func archiveURLFromEntry(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var tuple []json.RawMessage
	if json.Unmarshal(raw, &tuple) == nil && len(tuple) > 0 {
		if json.Unmarshal(tuple[0], &s) == nil {
			return s
		}
	}
	var obj struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.URL
	}
	return ""
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

// ── SSRF pre-flight / guarded outbound fetch ────────────────────────────────
//
// api.go's getClient is a shared HTTP client reused by trusted-caller routes
// and is hard-coded to netguard.DialControl(false) (only the cloud-metadata
// address is blocked); it is not touched here. archiveDownload and
// nzb.go's nzbCreate both fetch a caller-supplied URL for an unauthenticated
// route, so both validate the target host up front (validateFetchHost) and
// then fetch through archiveFetchClient below, whose dialer re-validates the
// actual resolved IP at connect time and whose redirect policy re-validates
// every hop — closing the DNS-rebinding / redirect-to-loopback gap that a
// pure pre-flight check cannot foreclose by itself.

// archiveAllowPrivateHosts reports whether STREMIO_ARCHIVE_ALLOW_PRIVATE
// opts /create endpoints (archive download, NZB fetch) into reaching
// private/loopback/RFC1918 hosts. Default is false (secure): only public
// addresses are reachable. The cloud-metadata address is always blocked
// regardless, via netguard.ValidateIP's unconditional check.
func archiveAllowPrivateHosts() bool {
	v := strings.TrimSpace(os.Getenv("STREMIO_ARCHIVE_ALLOW_PRIVATE"))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "on")
}

// archiveDialControl is archiveFetchClient's net.Dialer.Control hook. It
// re-reads archiveAllowPrivateHosts() on every dial — rather than baking the
// decision in once at package-init time — so the pooled client's behaviour
// always reflects the current opt-in state (mirrors ftpstream's
// ftpDialControl).
func archiveDialControl(network, address string, c syscall.RawConn) error {
	return netguard.DialControl(!archiveAllowPrivateHosts())(network, address, c)
}

// archiveMaxRedirects caps the number of redirects archiveFetchClient will
// follow, matching net/http's own default limit.
const archiveMaxRedirects = 5

// archiveCheckRedirect re-validates every redirect hop's target host before
// following it. Without this, a public URL that 30x-redirects to
// 127.0.0.1/RFC1918 would still be fetched: the dialer's Control hook only
// ever sees a resolved IP, so pairing it with a redirect-time host check
// (the same validateFetchHost used for the initial URL) closes the
// redirect-to-loopback gap, in addition to the dial-time DNS-rebinding
// guard.
func archiveCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= archiveMaxRedirects {
		return fmt.Errorf("stopped after %d redirects", archiveMaxRedirects)
	}
	if err := validateFetchHost(req.URL.String()); err != nil {
		return err
	}
	return nil
}

// archiveFetchClient is the dedicated HTTP client for /create-style outbound
// fetches (archive download, NZB XML fetch): its dialer enforces the SSRF
// guard at connect time (see archiveDialControl) and its CheckRedirect
// re-validates every redirect target, unlike api.go's getClient which is
// shared with trusted-caller routes and only blocks cloud-metadata.
var archiveFetchClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 10 * time.Second,
			Control: archiveDialControl,
		}).DialContext,
	},
	CheckRedirect: archiveCheckRedirect,
}

// archiveFetchGet performs a guarded GET against u via archiveFetchClient
// and returns up to maxBytes of the response body. Used by archiveDownload
// and nzb.go's nzbCreate (fetching the NZB XML) — both fetch a
// caller-supplied URL for an unauthenticated route, so neither may use
// api.go's getClient/httpGet.
func archiveFetchGet(u string, maxBytes int64) ([]byte, error) {
	resp, err := archiveFetchClient.Get(u)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", u, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxBytes))
}

// errFetchHostNotAllowed is returned by validateFetchHost for every blocked
// target, regardless of *why* the specific IP is blocked (cloud-metadata vs.
// private) or whether the host would otherwise be reachable. Callers surface
// this fixed message verbatim so the response never distinguishes "internal
// host exists and refused" from "internal host doesn't exist" from "internal
// host returned garbage" — closing the internal-port-scan oracle a
// differentiated error would otherwise provide.
var errFetchHostNotAllowed = errors.New("remote host not allowed")

// validateFetchHost resolves rawURL's host and rejects it when every
// candidate address is disallowed by netguard. It is a validate-then-fetch
// check (not a dial-time hook), so unlike netguard.DialControl it cannot by
// itself foreclose a DNS-rebinding TOCTOU window; it exists to close the
// coarse-grained "reach my LAN / cloud-metadata" SSRF on /create endpoints
// that download from a caller-supplied URL, which is the finding in scope.
// A DNS resolution failure is not itself treated as a block — the real fetch
// surfaces that error naturally, and it is not a distinguishing oracle here
// since it says nothing about the reachability of any internal address.
func validateFetchHost(rawURL string) error {
	if archiveAllowPrivateHosts() {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return errFetchHostNotAllowed
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
	if err != nil || len(addrs) == 0 {
		return nil
	}
	ips := make([]net.IP, len(addrs))
	for i, a := range addrs {
		ips[i] = a.IP
	}
	if anyAddrAllowed(ips) {
		return nil
	}
	return errFetchHostNotAllowed
}

// anyAddrAllowed reports whether at least one address in addrs is allowed by
// netguard.ValidateIP(ip, true) (public, i.e. neither cloud-metadata nor
// private/loopback/RFC1918). Factored out of validateFetchHost so the
// mixed-DNS-answer behavior — a hostname resolving to both a public and a
// private address passes this pre-flight check — is unit-testable with a
// literal []net.IP, without a real DNS lookup. This is intentionally a
// coarse pre-flight only: archiveFetchClient's dial-time Control hook is
// what actually forecloses the case where the connection ends up going to
// the disallowed address (DNS-rebinding or round-robin to the private
// answer).
func anyAddrAllowed(addrs []net.IP) bool {
	for _, ip := range addrs {
		if netguard.ValidateIP(ip, true) == nil {
			return true
		}
	}
	return false
}

// archiveLocalPathErrMsg is returned for every local-path rejection: local
// archives disabled, path escapes the configured root, or the path genuinely
// does not exist. Keeping the message and status identical across all three
// closes the filesystem-existence oracle (an attacker probing paths cannot
// distinguish "not configured" / "denied" / "not found").
const archiveLocalPathErrMsg = "local path not found"

// archiveLocalRoot returns the directory local archive sources are confined
// to, or "" when local-path archives are disabled (the default — without
// this the "url"/"from" field could name any file on disk; see
// archiveResolveLocalPath). STREMIO_ARCHIVE_LOCAL_ROOT is an archive-specific
// override; LOCAL_FILES_DIR (internal/api/localaddon.go's existing
// local-files-addon root) is reused as a fallback so operators who already
// mount a media directory get local-archive support without a second knob.
func archiveLocalRoot() string {
	if root := strings.TrimSpace(os.Getenv("STREMIO_ARCHIVE_LOCAL_ROOT")); root != "" {
		return root
	}
	return strings.TrimSpace(os.Getenv("LOCAL_FILES_DIR"))
}

// archiveResolveLocalPath confines source to archiveLocalRoot(), returning an
// error when local-path archives are disabled (no root configured) or when
// the resolved path escapes the root via ".." traversal or a symlink. Both
// the requested path and the configured root are resolved with
// filepath.EvalSymlinks so a symlink inside (or as) the root cannot be used
// to point outside it. When source itself does not exist yet,
// EvalSymlinks(source) fails, so its parent directory is resolved instead and
// the leaf name re-joined — the parent's symlink chain still cannot escape
// the root, and the subsequent os.Stat uniformly reports non-existence.
func archiveResolveLocalPath(source string) (string, error) {
	root := archiveLocalRoot()
	if root == "" {
		return "", errors.New("local archive sources are disabled")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", err
	}

	srcAbs, err := filepath.Abs(source)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(srcAbs)
	if err != nil {
		parent, perr := filepath.EvalSymlinks(filepath.Dir(srcAbs))
		if perr != nil {
			return "", perr
		}
		real = filepath.Join(parent, filepath.Base(srcAbs))
	}

	rel, err := filepath.Rel(rootReal, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes allowed root")
	}
	return real, nil
}

// ── helper: download / key / path-encoding ───────────────────────────────────

// archiveDownload fetches u and streams it to a new temp file.
// The caller owns the file and must remove it when done.
func archiveDownload(u string) (string, error) {
	if err := validateFetchHost(u); err != nil {
		return "", err
	}
	resp, err := archiveFetchClient.Get(u)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: status %d", u, resp.StatusCode)
	}
	f, err := os.CreateTemp("", "stremio-archive-dl-*")
	if err != nil {
		return "", err
	}
	// Cap at archiveMaxDownloadBytes; use +1 so we can detect an overflow by
	// checking whether we copied strictly more than the limit.
	n, copyErr := io.Copy(f, io.LimitReader(resp.Body, archiveMaxDownloadBytes+1))
	if copyErr != nil || n > archiveMaxDownloadBytes {
		_ = f.Close()
		_ = os.Remove(f.Name())
		if copyErr != nil {
			return "", fmt.Errorf("download %s: %w", u, copyErr)
		}
		return "", fmt.Errorf("download %s: response exceeds size limit (%d bytes)", u, archiveMaxDownloadBytes)
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
	defer func() { _ = r.Close() }()

	// Obtain the declared (uncompressed) size from the archive index. This is
	// used below to cap the extraction and prevent zip-bomb decompression.
	entries, err := r.List()
	if err != nil {
		return "", fmt.Errorf("list archive: %w", err)
	}
	var declaredSize int64 = -1
	for _, e := range entries {
		if e.Name == entryName {
			declaredSize = e.Size
			break
		}
	}
	if declaredSize < 0 {
		return "", fmt.Errorf("entry %q not found in archive", entryName)
	}
	if declaredSize > archiveMaxEntryBytes {
		// Reject before opening the entry to avoid unnecessary I/O; rc is not
		// yet open at this point (r.Open is called below).
		return "", fmt.Errorf("entry %q: declared size %d exceeds limit %d bytes", entryName, declaredSize, archiveMaxEntryBytes)
	}

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

	// Use LimitReader(rc, declaredSize+1) to enforce the zip-bomb guard:
	// at most declaredSize bytes are written; if the source yields more we
	// remove the partial file and return an error.
	n, copyErr := io.Copy(f, io.LimitReader(rc, declaredSize+1))
	_ = rc.Close()
	_ = f.Close()
	if copyErr != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("extract %q: %w", entryName, copyErr)
	}
	if n > declaredSize {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("extract %q: content (%d bytes) exceeds declared size (%d bytes)", entryName, n, declaredSize)
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
	source := payload.source()
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
		resolved, resolveErr := archiveResolveLocalPath(source)
		if resolveErr != nil {
			http.Error(w, archiveLocalPathErrMsg, http.StatusBadRequest)
			return
		}
		if _, statErr := os.Stat(resolved); statErr != nil {
			http.Error(w, archiveLocalPathErrMsg, http.StatusBadRequest)
			return
		}
		archivePath = resolved
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
	if old, ok := archiveSessions[key]; ok && old.tmpDir != "" {
		// A prior session used this key. Remove its temp dir (and, if the
		// archive itself was a downloaded temp file, the archive too)
		// asynchronously to avoid leaking disk space without blocking the lock.
		oldIsTempArch := old.isTempArch
		oldArchivePath := old.archivePath
		go func(d, archPath string, isTmp bool) {
			_ = os.RemoveAll(d)
			if isTmp {
				_ = os.Remove(archPath)
			}
		}(old.tmpDir, oldArchivePath, oldIsTempArch)
	}
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

	sess.mu.Lock()
	sess.lastAccess = time.Now()
	sess.refCount++
	sess.mu.Unlock()
	defer func() {
		sess.mu.Lock()
		sess.refCount--
		sess.mu.Unlock()
	}()

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
	defer func() { _ = f.Close() }()

	w.Header().Set("Content-Type", mimeByName(entryName))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("transferMode.dlna.org", "Streaming")
	w.Header().Set("contentFeatures.dlna.org",
		"DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000")

	http.ServeContent(w, r, entryName, time.Time{}, f)
}
