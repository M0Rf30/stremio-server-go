package media

// Tests for pure and offline-testable helpers in internal/media.
//
// Excluded (require real ffprobe/ffmpeg invocation):
//   - Probe, Tracks, probeMedia, transcodeSegment, HLSFile (segment transcode)
//   - verifyEncoder, selectEncoder, encodersList, extractSubtitle

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── test helper ───────────────────────────────────────────────────────────────

// newTestHLSManager constructs an hlsManager with a temp base dir and no
// running reaper goroutine.  This avoids calling newHLS() (which invokes
// ffmpeg to probe encoders).
func newTestHLSManager(t *testing.T) *hlsManager {
	t.Helper()
	return &hlsManager{
		base:         t.TempDir(),
		sessions:     map[string]*hlsSession{},
		probeCache:   map[string]probeCacheEntry{},
		transcodeSem: make(chan struct{}, 1),
		stopCh:       make(chan struct{}),
	}
}

// ── hls.go: sanitizeM3U8Attr ──────────────────────────────────────────────────

func TestSanitizeM3U8Attr(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"clean", "English", "English"},
		// Title with embedded double-quotes (e.g. English "HD") — quotes stripped.
		{"title with double quotes", `English "HD"`, "English HD"},
		// Newline in the middle of a name.
		{"title with LF", "English\nHD", "EnglishHD"},
		// CR+LF Windows line ending — both bytes removed.
		{"title with CRLF", "English\r\nHD", "EnglishHD"},
		// Comma would break the M3U8 attribute list.
		{"title with comma", "English, HD", "English HD"},
		// All four illegal chars together.
		{"all illegal chars", "\",\r\n", ""},
		// Already clean with spaces and dashes.
		{"clean with spaces", "English - HD", "English - HD"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeM3U8Attr(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeM3U8Attr(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ── hls.go: localize ──────────────────────────────────────────────────────────

func TestLocalize(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://127.0.0.1:12470/stream", "http://127.0.0.1:11470/stream"},
		{"http://127.0.0.1:11470/stream", "http://127.0.0.1:11470/stream"},    // already local http
		{"https://example.com/stream", "https://example.com/stream"},          // external — unchanged
		{"https://127.0.0.1:12470/a/b?c=d", "http://127.0.0.1:11470/a/b?c=d"}, // path + query
		{"", ""},
	}
	for _, tc := range cases {
		got := localize(tc.in)
		if got != tc.want {
			t.Errorf("localize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ── hls.go: ftoa ─────────────────────────────────────────────────────────────

func TestFtoa(t *testing.T) {
	cases := []struct {
		f    float64
		want string
	}{
		{0, "0.000"},
		{4.0, "4.000"},
		{3.14159, "3.142"},
		{1.5, "1.500"},
		{120, "120.000"},
	}
	for _, tc := range cases {
		got := ftoa(tc.f)
		if got != tc.want {
			t.Errorf("ftoa(%v) = %q, want %q", tc.f, got, tc.want)
		}
	}
}

// ── hls.go: hlsSession.lockFor ────────────────────────────────────────────────

func TestHLSSessionLockFor(t *testing.T) {
	s := &hlsSession{segLocks: map[string]*sync.Mutex{}}

	// Same filename → same mutex pointer.
	mu1 := s.lockFor("seg0.ts")
	mu2 := s.lockFor("seg0.ts")
	if mu1 != mu2 {
		t.Error("lockFor returned different mutexes for the same filename")
	}

	// Different filename → different mutex.
	mu3 := s.lockFor("a1seg0.ts")
	if mu1 == mu3 {
		t.Error("lockFor returned the same mutex for different filenames")
	}

	// Mutex must be usable.
	if !mu1.TryLock() {
		t.Error("expected to acquire freshly returned mutex")
	}
	mu1.Unlock()
}

// ── hls.go: hlsSession.writePlaylist ─────────────────────────────────────────

func TestHLSSessionWritePlaylist(t *testing.T) {
	dir := t.TempDir()
	s := &hlsSession{
		dir:      dir,
		duration: 10.0, // 10 s → ceil(10/4) = 3 segments
		segLocks: map[string]*sync.Mutex{},
	}

	path := filepath.Join(dir, "playlist.m3u8")
	if err := s.writePlaylist(path, ""); err != nil {
		t.Fatalf("writePlaylist: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	content := string(raw)

	for _, want := range []string{
		"#EXTM3U",
		"#EXT-X-VERSION:3",
		"#EXT-X-PLAYLIST-TYPE:VOD",
		"#EXT-X-ENDLIST",
		"seg0.ts",
		"seg1.ts",
		"seg2.ts",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("writePlaylist output missing %q:\n%s", want, content)
		}
	}

	// Duration 0 must return an error.
	s2 := &hlsSession{dir: dir, duration: 0, segLocks: map[string]*sync.Mutex{}}
	if err := s2.writePlaylist(filepath.Join(dir, "bad.m3u8"), ""); err == nil {
		t.Error("writePlaylist with duration=0 should return an error")
	}
}

func TestHLSSessionWritePlaylistAudioPrefix(t *testing.T) {
	dir := t.TempDir()
	s := &hlsSession{dir: dir, duration: 8.0, segLocks: map[string]*sync.Mutex{}}
	path := filepath.Join(dir, "audio1.m3u8")
	if err := s.writePlaylist(path, "a1"); err != nil {
		t.Fatalf("writePlaylist: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "a1seg0.ts") {
		t.Errorf("prefix not applied: %s", raw)
	}
}

// ── hls.go: hlsSession.writeSubPlaylist ──────────────────────────────────────

func TestHLSSessionWriteSubPlaylist(t *testing.T) {
	dir := t.TempDir()
	s := &hlsSession{dir: dir, duration: 90.0, segLocks: map[string]*sync.Mutex{}}
	path := filepath.Join(dir, "sub0.m3u8")
	if err := s.writeSubPlaylist(path, 0); err != nil {
		t.Fatalf("writeSubPlaylist: %v", err)
	}
	raw, _ := os.ReadFile(path)
	content := string(raw)
	for _, want := range []string{"#EXTM3U", "#EXT-X-ENDLIST", "sub0.vtt"} {
		if !strings.Contains(content, want) {
			t.Errorf("writeSubPlaylist missing %q:\n%s", want, content)
		}
	}
}

// ── hls.go: hlsManager lifecycle ─────────────────────────────────────────────

func TestHLSManagerIdGuard(t *testing.T) {
	m := newTestHLSManager(t)
	base := m.base

	badIDs := []string{
		".",
		"..",
		"../escape",
		"foo/bar",
		"a/..b",
		"foo..bar",    // contains ".."
		"",            // empty
		"/etc/passwd", // absolute
	}

	for _, id := range badIDs {
		t.Run(fmt.Sprintf("id=%q", id), func(t *testing.T) {
			_, err := m.StartHLS(id, "http://127.0.0.1:1/dummy.mkv")
			if err == nil {
				t.Errorf("StartHLS(%q) should have returned an error", id)
			}
		})
	}

	// Regression: base dir must still exist after all rejected calls.
	if _, err := os.Stat(base); os.IsNotExist(err) {
		t.Error("regression: base dir was removed after id-guard rejection")
	}
}

func TestHLSManagerValidIdCreatesDir(t *testing.T) {
	// Serve garbage so ffprobe fails fast (no valid media format detected).
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not a media file"))
	}))
	defer ts.Close()

	m := newTestHLSManager(t)

	// A valid id must not be rejected and the session dir must be created.
	// StartHLS will call ffprobe on ts.URL which fails quickly; the dir is
	// created before probing so the assertion is valid regardless of probe outcome.
	_, err := m.StartHLS("sess-abc", ts.URL+"/dummy.mkv")
	if err != nil {
		t.Fatalf("StartHLS with valid id should not return error (probe failure is silent), got: %v", err)
	}

	sessDir := filepath.Join(m.base, "sess-abc")
	if _, statErr := os.Stat(sessDir); os.IsNotExist(statErr) {
		t.Error("session dir was not created for valid id")
	}
}

func TestHLSManagerSessions(t *testing.T) {
	m := newTestHLSManager(t)
	if n := m.Sessions(); n != 0 {
		t.Errorf("initial Sessions() = %d, want 0", n)
	}

	// Directly inject a session (bypassing probeMedia).
	m.mu.Lock()
	m.sessions["s1"] = &hlsSession{segLocks: map[string]*sync.Mutex{}}
	m.mu.Unlock()

	if n := m.Sessions(); n != 1 {
		t.Errorf("Sessions() after add = %d, want 1", n)
	}
}

func TestHLSManagerEvictIdle(t *testing.T) {
	m := newTestHLSManager(t)

	// Expired session: last access well in the past.
	dirExpired := filepath.Join(m.base, "expired")
	_ = os.MkdirAll(dirExpired, 0o755)
	sessExpired := &hlsSession{dir: dirExpired, segLocks: map[string]*sync.Mutex{}}
	sessExpired.lastAccess.Store(time.Now().Add(-2 * sessionTTL).UnixNano())

	// Fresh session: just accessed.
	dirFresh := filepath.Join(m.base, "fresh")
	_ = os.MkdirAll(dirFresh, 0o755)
	sessFresh := &hlsSession{dir: dirFresh, segLocks: map[string]*sync.Mutex{}}
	sessFresh.lastAccess.Store(time.Now().UnixNano())

	m.mu.Lock()
	m.sessions["expired"] = sessExpired
	m.sessions["fresh"] = sessFresh
	m.mu.Unlock()

	m.evictIdle()

	// Expired session and its dir must be gone.
	if _, err := os.Stat(dirExpired); !os.IsNotExist(err) {
		t.Error("evictIdle did not remove expired session dir")
	}
	m.mu.Lock()
	_, hasExpired := m.sessions["expired"]
	_, hasFresh := m.sessions["fresh"]
	m.mu.Unlock()
	if hasExpired {
		t.Error("evictIdle did not remove expired session from map")
	}
	// Fresh session must be kept.
	if !hasFresh {
		t.Error("evictIdle removed a fresh session")
	}
	if _, err := os.Stat(dirFresh); os.IsNotExist(err) {
		t.Error("evictIdle removed fresh session dir")
	}
}

// TestHLSManagerBaseDirSurvivesCloseHLS is the regression test for the
// base-dir-wipe bug: CloseHLS must remove session sub-dirs but not the base.
func TestHLSManagerBaseDirSurvivesCloseHLS(t *testing.T) {
	m := newTestHLSManager(t)
	base := m.base

	// Manually create a session with a sub-directory.
	sessDir := filepath.Join(base, "sess-cleanup")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	sess := &hlsSession{dir: sessDir, segLocks: map[string]*sync.Mutex{}}
	sess.lastAccess.Store(time.Now().UnixNano())
	m.sessions["sess-cleanup"] = sess
	m.mu.Unlock()

	m.CloseHLS()

	// Regression: base must survive.
	if _, err := os.Stat(base); os.IsNotExist(err) {
		t.Error("regression: CloseHLS wiped the base dir (should only remove session sub-dirs)")
	}
	// Session dir must be removed.
	if _, err := os.Stat(sessDir); !os.IsNotExist(err) {
		t.Error("CloseHLS did not remove session dir")
	}
}

// ── media.go: isHTTP / toLocalPath ───────────────────────────────────────────

func TestIsHTTP(t *testing.T) {
	cases := []struct {
		u    string
		want bool
	}{
		{"http://example.com/video.mp4", true},
		{"https://example.com/video.mp4", true},
		{"file:///tmp/video.mp4", false},
		{"/tmp/video.mp4", false},
		{"ftp://example.com/video.mp4", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isHTTP(tc.u)
		if got != tc.want {
			t.Errorf("isHTTP(%q) = %v, want %v", tc.u, got, tc.want)
		}
	}
}

func TestToLocalPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"file:///tmp/video.mp4", "/tmp/video.mp4"},
		{"/tmp/video.mp4", "/tmp/video.mp4"},
		{"file://video.mp4", "video.mp4"},
		{"", ""},
	}
	for _, tc := range cases {
		got := toLocalPath(tc.in)
		if got != tc.want {
			t.Errorf("toLocalPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ── media.go: Probe/Tracks cache hard-cap eviction ───────────────────────────

// newTestProber constructs a prober with initialized caches but no hlsManager
// (Probe and Tracks never touch p.hls).
func newTestProber() *prober {
	return &prober{
		probeCache:  map[string]probeResultEntry{},
		tracksCache: map[string]tracksCacheEntry{},
	}
}

// TestProbeCacheHardCap is the regression test for the unbounded-growth bug:
// a burst of distinct URLs, all still within their TTL, must never push
// p.probeCache past probeTracksCacheMaxSize.  http://127.0.0.1:1 is a
// privileged, unused port, so ffprobe fails near-instantly on "connection
// refused" with no real network I/O — deterministic and offline whether or
// not the ffprobe binary is even installed (a missing binary fails just as
// fast); only the resulting cache size is under test, not ffprobe's output.
func TestProbeCacheHardCap(t *testing.T) {
	p := newTestProber()
	notExpired := time.Now().Add(time.Hour)
	for i := range probeTracksCacheMaxSize {
		p.probeCache[fmt.Sprintf("http://127.0.0.1:1/pre%d", i)] = probeResultEntry{expiresAt: notExpired}
	}

	if _, err := p.Probe("http://127.0.0.1:1/new"); err == nil {
		t.Fatal("Probe against a refused port unexpectedly succeeded")
	}

	if got := len(p.probeCache); got > probeTracksCacheMaxSize {
		t.Errorf("len(probeCache) = %d, want <= %d (hard cap not enforced)", got, probeTracksCacheMaxSize)
	}
}

// TestTracksCacheHardCap mirrors TestProbeCacheHardCap for p.tracksCache.
func TestTracksCacheHardCap(t *testing.T) {
	p := newTestProber()
	notExpired := time.Now().Add(time.Hour)
	for i := range probeTracksCacheMaxSize {
		p.tracksCache[fmt.Sprintf("http://127.0.0.1:1/pre%d", i)] = tracksCacheEntry{expiresAt: notExpired}
	}

	if _, err := p.Tracks("http://127.0.0.1:1/new"); err == nil {
		t.Fatal("Tracks against a refused port unexpectedly succeeded")
	}

	if got := len(p.tracksCache); got > probeTracksCacheMaxSize {
		t.Errorf("len(tracksCache) = %d, want <= %d (hard cap not enforced)", got, probeTracksCacheMaxSize)
	}
}

// ── media.go: computeOpenSubHash ─────────────────────────────────────────────

func TestComputeOpenSubHash(t *testing.T) {
	// Regression constant: pre-computed with known inputs.
	//
	// size=1000, head=[1,0,0,0,0,0,0,0] (LE uint64=1),
	//            tail=[2,0,0,0,0,0,0,0] (LE uint64=2)
	// → h = 1000 + 1 + 2 = 1003 = 0x00000000000003eb
	const wantConst = uint64(1003)
	head := []byte{1, 0, 0, 0, 0, 0, 0, 0}
	tail := []byte{2, 0, 0, 0, 0, 0, 0, 0}
	got := computeOpenSubHash(1000, head, tail)
	if got != wantConst {
		t.Errorf("regression constant failed: got %d (0x%016x), want %d", got, got, wantConst)
	}

	// Formatted output must be 16 lowercase hex digits.
	hex := fmt.Sprintf("%016x", got)
	if hex != "00000000000003eb" {
		t.Errorf("hex format: got %q, want %q", hex, "00000000000003eb")
	}

	// All-zero buffers: hash equals size.
	zeroHead := make([]byte, 8)
	zeroTail := make([]byte, 8)
	for _, size := range []int64{0, 1, 99, 65536, 1<<32 - 1} {
		h := computeOpenSubHash(size, zeroHead, zeroTail)
		if h != uint64(size) {
			t.Errorf("all-zero buffers with size=%d: got %d, want %d", size, h, size)
		}
	}

	// Size must contribute: same buffers, different sizes → different hashes.
	h1 := computeOpenSubHash(100, head, tail)
	h2 := computeOpenSubHash(101, head, tail)
	if h1 == h2 {
		t.Error("size does not contribute to hash: h(100)==h(101)")
	}

	// Buffer content must contribute.
	h3 := computeOpenSubHash(100, make([]byte, 8), make([]byte, 8))
	if h1 == h3 {
		t.Error("buffer content does not contribute to hash")
	}

	// Natural uint64 overflow must not panic.
	maxBuf := make([]byte, 8)
	for i := range maxBuf {
		maxBuf[i] = 0xff
	}
	_ = computeOpenSubHash(math.MaxInt64, maxBuf, maxBuf)
}

// ── media.go: OpenSubHash (local file path) ───────────────────────────────────

func TestOpenSubHashLocalFile(t *testing.T) {
	const fileSize = 200_000 // larger than 2 × chunkSize (2×65536=131072)
	data := make([]byte, fileSize)
	for i := range data {
		data[i] = byte(i % 251) // prime modulus gives non-trivial pattern
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "video.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	p := &prober{} // hls not needed for OpenSubHash
	result, err := p.OpenSubHash(path)
	if err != nil {
		t.Fatalf("OpenSubHash: %v", err)
	}

	m, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("result type = %T, want map[string]interface{}", result)
	}

	hash, _ := m["hash"].(string)
	size, _ := m["size"].(int64)

	if len(hash) != 16 {
		t.Errorf("hash len = %d, want 16", len(hash))
	}
	if size != fileSize {
		t.Errorf("size = %d, want %d", size, fileSize)
	}

	// Must be stable (same file → same hash).
	result2, err := p.OpenSubHash(path)
	if err != nil {
		t.Fatalf("OpenSubHash second call: %v", err)
	}
	m2 := result2.(map[string]interface{})
	if m2["hash"] != hash {
		t.Error("hash is not stable across calls")
	}

	// Verify against reference computation.
	wantHash := fmt.Sprintf("%016x", computeOpenSubHash(
		int64(fileSize),
		data[:chunkSize],
		data[fileSize-chunkSize:],
	))
	if hash != wantHash {
		t.Errorf("hash = %q, want %q", hash, wantHash)
	}
}

func TestOpenSubHashLocalFileSmall(t *testing.T) {
	// File smaller than chunkSize: head==tail in readLocalChunks.
	data := []byte("small file content")
	dir := t.TempDir()
	path := filepath.Join(dir, "small.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	p := &prober{}
	result, err := p.OpenSubHash(path)
	if err != nil {
		t.Fatalf("OpenSubHash: %v", err)
	}
	m := result.(map[string]interface{})
	if m["size"].(int64) != int64(len(data)) {
		t.Errorf("size = %d, want %d", m["size"], len(data))
	}
	if len(m["hash"].(string)) != 16 {
		t.Error("hash must be 16 hex chars")
	}
}

// ── media.go: OpenSubHash via HTTP (exercises fetchHTTPChunks/httpRangeGet) ──

func TestOpenSubHashHTTP(t *testing.T) {
	const fileSize = 200_000
	content := make([]byte, fileSize)
	for i := range content {
		content[i] = byte(i % 127)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// http.ServeContent handles HEAD, Range GET, and Content-Length automatically.
		http.ServeContent(w, r, "video.bin", time.Time{}, bytes.NewReader(content))
	}))
	defer ts.Close()

	p := &prober{}
	result, err := p.OpenSubHash(ts.URL + "/video.bin")
	if err != nil {
		t.Fatalf("OpenSubHash HTTP: %v", err)
	}
	m := result.(map[string]interface{})

	hash, _ := m["hash"].(string)
	size, _ := m["size"].(int64)

	if len(hash) != 16 {
		t.Errorf("hash len = %d, want 16", len(hash))
	}
	if size != fileSize {
		t.Errorf("size = %d, want %d", size, fileSize)
	}

	// Must agree with the local-file computation.
	wantHash := fmt.Sprintf("%016x", computeOpenSubHash(
		int64(fileSize),
		content[:chunkSize],
		content[fileSize-chunkSize:],
	))
	if hash != wantHash {
		t.Errorf("HTTP hash = %q, want %q (from local computation)", hash, wantHash)
	}
}

// ── media.go: readLocalChunks ────────────────────────────────────────────────

func TestReadLocalChunksLargeFile(t *testing.T) {
	const size = 3 * chunkSize // head and tail are distinct regions
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i)
	}
	path := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	gotSize, head, tail, err := readLocalChunks(path)
	if err != nil {
		t.Fatalf("readLocalChunks: %v", err)
	}
	if gotSize != size {
		t.Errorf("size = %d, want %d", gotSize, size)
	}
	if len(head) != chunkSize {
		t.Errorf("head len = %d, want %d", len(head), chunkSize)
	}
	if len(tail) != chunkSize {
		t.Errorf("tail len = %d, want %d", len(tail), chunkSize)
	}
	// head must be the first 64 KiB.
	if !bytes.Equal(head, data[:chunkSize]) {
		t.Error("head does not match first chunkSize bytes")
	}
	// tail must be the last 64 KiB.
	if !bytes.Equal(tail, data[size-chunkSize:]) {
		t.Error("tail does not match last chunkSize bytes")
	}
}

func TestReadLocalChunksSmallFile(t *testing.T) {
	data := []byte("tiny")
	path := filepath.Join(t.TempDir(), "tiny.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	gotSize, head, tail, err := readLocalChunks(path)
	if err != nil {
		t.Fatalf("readLocalChunks: %v", err)
	}
	if gotSize != int64(len(data)) {
		t.Errorf("size = %d, want %d", gotSize, len(data))
	}
	// Both head and tail should be the same slice content (same small data).
	if !bytes.Equal(head, data) || !bytes.Equal(tail, data) {
		t.Error("head or tail does not match small file data")
	}
}

// ── subtitles.go: pure helpers ───────────────────────────────────────────────

func TestFmtTimestamp(t *testing.T) {
	cases := []struct {
		ms    int
		isVTT bool
		want  string
	}{
		{0, false, "00:00:00,000"},
		{0, true, "00:00:00.000"},
		{1000, false, "00:00:01,000"},
		{1001, false, "00:00:01,001"},
		{60000, false, "00:01:00,000"},
		{3600000, false, "01:00:00,000"},
		{3661500, false, "01:01:01,500"},
		{3661500, true, "01:01:01.500"},
		{-1, false, "00:00:00,000"}, // clamped to 0
		{999, true, "00:00:00.999"},
	}
	for _, tc := range cases {
		got := fmtTimestamp(tc.ms, tc.isVTT)
		if got != tc.want {
			t.Errorf("fmtTimestamp(%d, isVTT=%v) = %q, want %q", tc.ms, tc.isVTT, got, tc.want)
		}
	}
}

func TestParseTimestamp(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{"00:00:01,000", 1000},
		{"00:00:01.000", 1000},    // VTT dot separator
		{"01:00:00,000", 3600000}, // one hour
		{"01:02:03,456", 3723456},
		{"02:30,500", 150500},             // MM:SS,mmm form
		{"00:00:01,000 align:left", 1000}, // WEBVTT cue settings discarded
	}
	for _, tc := range cases {
		got := parseTimestamp(tc.s)
		if got != tc.want {
			t.Errorf("parseTimestamp(%q) = %d, want %d", tc.s, got, tc.want)
		}
	}
}

func TestParseSecMs(t *testing.T) {
	cases := []struct {
		s       string
		wantSec int
		wantMs  int
	}{
		{"01.500", 1, 500},
		{"00.000", 0, 0},
		{"59.999", 59, 999},
		{"10", 10, 0}, // no decimal part
	}
	for _, tc := range cases {
		sec, ms := parseSecMs(tc.s)
		if sec != tc.wantSec || ms != tc.wantMs {
			t.Errorf("parseSecMs(%q) = (%d, %d), want (%d, %d)", tc.s, sec, ms, tc.wantSec, tc.wantMs)
		}
	}
}

func TestParseTimestampRoundTrip(t *testing.T) {
	// fmtTimestamp and parseTimestamp must be inverses for valid timestamps.
	for _, ms := range []int{0, 1, 999, 1000, 3661500, 86399999} {
		for _, isVTT := range []bool{false, true} {
			formatted := fmtTimestamp(ms, isVTT)
			got := parseTimestamp(formatted)
			if got != ms {
				t.Errorf("round-trip (isVTT=%v): ms=%d → %q → %d", isVTT, ms, formatted, got)
			}
		}
	}
}

func TestParseASSTimestamp(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{"0:00:01.00", 1000},    // 1 second, 0 centiseconds
		{"0:01:00.00", 60000},   // 1 minute
		{"1:00:00.00", 3600000}, // 1 hour
		{"0:00:00.50", 500},     // 50 centiseconds = 500 ms
		{"0:00:01.25", 1250},    // 1s + 25cs = 1250 ms
		{"2:30:45.12", 9045120}, // 2h+30m+45s+12cs = 9045120 ms
	}
	for _, tc := range cases {
		got := parseASSTimestamp(tc.s)
		if got != tc.want {
			t.Errorf("parseASSTimestamp(%q) = %d, want %d", tc.s, got, tc.want)
		}
	}
}

func TestStripASSMarkup(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"{\\an8}Hello", "Hello"},                        // positional tag
		{"{\\b1}bold{\\b0}", "bold"},                     // bold on/off
		{"Hello\\NWorld", "Hello\nWorld"},                // soft break \N
		{"Hello\\nWorld", "Hello\nWorld"},                // soft break \n
		{"No tags here", "No tags here"},                 // passthrough
		{"{unclosed", ""},                                // unclosed tag: skip to end
		{"{\\an8}Hi\\NThere{\\c&HFF0000&}", "Hi\nThere"}, // mixed
		{"\\\\escaped", "\\\\escaped"},                   // non-N backslash passthrough
		{"", ""},
	}
	for _, tc := range cases {
		got := stripASSMarkup(tc.in)
		if got != tc.want {
			t.Errorf("stripASSMarkup(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLooksLikeBinary(t *testing.T) {
	// Text content: no null bytes → not binary.
	if looksLikeBinary([]byte("1\n00:00:01,000 --> 00:00:02,000\nHello")) {
		t.Error("plain SRT text incorrectly flagged as binary")
	}
	// Null byte → binary.
	if !looksLikeBinary([]byte("PGS\x00data")) {
		t.Error("data with null byte should be flagged as binary")
	}
	// Null byte beyond first 512 bytes → not flagged (check window is 512 bytes).
	data := make([]byte, 600)
	for i := range data {
		data[i] = 'A'
	}
	data[513] = 0x00 // outside the check window
	if looksLikeBinary(data) {
		t.Error("null byte after 512 bytes should not trigger binary flag")
	}
	// Null byte at exactly byte 511 (within window) → binary.
	data2 := make([]byte, 600)
	for i := range data2 {
		data2[i] = 'B'
	}
	data2[511] = 0x00
	if !looksLikeBinary(data2) {
		t.Error("null byte at index 511 should trigger binary flag")
	}
}

func TestIsASS(t *testing.T) {
	assScriptInfo := "[Script Info]\nScriptType: v4.00+\n[Events]\n"
	assEvents := "[Events]\nFormat: Layer, Start, End, Style, Name\n"
	ssaScript := "ScriptType: v4.00\n"
	notASS := "1\n00:00:01,000 --> 00:00:02,000\nHello"

	if !isASS(assScriptInfo) {
		t.Error("[Script Info] should be detected as ASS")
	}
	if !isASS(assEvents) {
		t.Error("[Events] should be detected as ASS")
	}
	if !isASS(ssaScript) {
		t.Error("ScriptType: v4 should be detected as ASS")
	}
	if isASS(notASS) {
		t.Error("SRT content should not be detected as ASS")
	}
}

// ── subtitles.go: parseSubtitles ─────────────────────────────────────────────

func TestParseSubtitlesSRT(t *testing.T) {
	srt := "1\n00:00:01,000 --> 00:00:02,000\nHello World\n\n" +
		"2\n00:00:03,500 --> 00:00:05,000\nSecond cue\nwith two lines\n\n"

	tracks := parseSubtitles([]byte(srt))
	if len(tracks) != 2 {
		t.Fatalf("expected 2 tracks, got %d", len(tracks))
	}
	if tracks[0]["startTime"] != 1000 || tracks[0]["endTime"] != 2000 {
		t.Errorf("track 0 timestamps wrong: %v", tracks[0])
	}
	if tracks[0]["text"] != "Hello World" {
		t.Errorf("track 0 text = %q", tracks[0]["text"])
	}
	if tracks[1]["text"] != "Second cue\nwith two lines" {
		t.Errorf("track 1 text = %q", tracks[1]["text"])
	}
}

func TestParseSubtitlesVTT(t *testing.T) {
	vtt := "WEBVTT\n\n" +
		"00:00:01.000 --> 00:00:02.500\nHello VTT\n\n" +
		"NOTE this block has no cue text and should be skipped\n\n" +
		"00:00:05.000 --> 00:00:06.000 align:left\nCue with settings\n\n"

	tracks := parseSubtitles([]byte(vtt))
	if len(tracks) != 2 {
		t.Fatalf("expected 2 tracks, got %d: %v", len(tracks), tracks)
	}
	if tracks[0]["startTime"] != 1000 || tracks[0]["endTime"] != 2500 {
		t.Errorf("track 0 timestamps wrong: %v", tracks[0])
	}
	if tracks[0]["text"] != "Hello VTT" {
		t.Errorf("track 0 text = %q", tracks[0]["text"])
	}
	// VTT cue settings (align:left) must be discarded from the timestamp.
	if tracks[1]["startTime"] != 5000 {
		t.Errorf("track 1 startTime = %v, want 5000", tracks[1]["startTime"])
	}
}

func TestParseSubtitlesBOM(t *testing.T) {
	// UTF-8 BOM prefix must be stripped.
	srt := "\xEF\xBB\xBF1\n00:00:00,100 --> 00:00:01,000\nBOM test\n\n"
	tracks := parseSubtitles([]byte(srt))
	if len(tracks) != 1 {
		t.Fatalf("expected 1 track, got %d", len(tracks))
	}
}

func TestParseSubtitlesBinary(t *testing.T) {
	// Null byte in data → treated as binary → returns nil.
	binary := []byte("PGS\x00binary subtitle payload")
	tracks := parseSubtitles(binary)
	if tracks != nil {
		t.Errorf("binary data should return nil tracks, got %v", tracks)
	}
}

func TestParseSubtitlesEmpty(t *testing.T) {
	tracks := parseSubtitles([]byte(""))
	if len(tracks) != 0 {
		t.Errorf("empty input: expected 0 tracks, got %d", len(tracks))
	}
}

// ── subtitles.go: parseASSSubtitles ──────────────────────────────────────────

func TestParseASSSubtitles(t *testing.T) {
	ass := `[Script Info]
ScriptType: v4.00+

[V4+ Styles]
Format: Name, Fontname, Fontsize
Style: Default,Arial,20

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:01.00,0:00:02.50,Default,,0,0,0,,Hello {\\b1}World
Dialogue: 0,0:00:05.00,0:00:06.00,Default,,0,0,0,,Second\\Nline
`

	tracks := parseASSSubtitles(ass)
	if len(tracks) != 2 {
		t.Fatalf("expected 2 tracks, got %d: %v", len(tracks), tracks)
	}
	if tracks[0]["startTime"] != 1000 || tracks[0]["endTime"] != 2500 {
		t.Errorf("track 0 timestamps: start=%v end=%v", tracks[0]["startTime"], tracks[0]["endTime"])
	}
	text0, _ := tracks[0]["text"].(string)
	if !strings.Contains(text0, "Hello") || !strings.Contains(text0, "World") {
		t.Errorf("track 0 text = %q, want to contain 'Hello World'", text0)
	}
	text1, _ := tracks[1]["text"].(string)
	if !strings.Contains(text1, "\n") {
		t.Errorf("track 1 text = %q, want soft line break converted to newline", text1)
	}
}

func TestParseSubtitlesASS(t *testing.T) {
	// parseSubtitles auto-dispatches to parseASSSubtitles for ASS content.
	ass := "[Script Info]\nScriptType: v4.00+\n\n[Events]\n" +
		"Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n" +
		"Dialogue: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,Via parseSubtitles\n"

	tracks := parseSubtitles([]byte(ass))
	if len(tracks) != 1 {
		t.Fatalf("expected 1 track, got %d", len(tracks))
	}
}

// ── subtitles.go: fetchSubBytes and friends (via httptest) ───────────────────

func TestFetchSubBytesRejectsNonHTTP(t *testing.T) {
	_, err := fetchSubBytes("/local/path.srt")
	if err == nil {
		t.Error("fetchSubBytes should reject non-http paths")
	}
	_, err = fetchSubBytes("file:///tmp/test.srt")
	if err == nil {
		t.Error("fetchSubBytes should reject file:// scheme")
	}
}

func TestSubtitlesTracks(t *testing.T) {
	srt := "1\n00:00:01,000 --> 00:00:03,000\nTest cue\n\n"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, srt)
	}))
	defer ts.Close()

	p := &prober{}
	result, err := p.SubtitlesTracks(ts.URL + "/sub.srt")
	if err != nil {
		t.Fatalf("SubtitlesTracks: %v", err)
	}
	m, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("result type = %T", result)
	}
	tracksRaw, ok := m["tracks"]
	if !ok {
		t.Fatal("result missing 'tracks' key")
	}
	tracks, ok := tracksRaw.([]map[string]interface{})
	if !ok {
		t.Fatalf("tracks type = %T", tracksRaw)
	}
	if len(tracks) != 1 {
		t.Fatalf("expected 1 track, got %d", len(tracks))
	}
	if tracks[0]["startTime"] != 1000 || tracks[0]["endTime"] != 3000 {
		t.Errorf("track timestamps: %v", tracks[0])
	}
}

func TestWriteSubtitlesSRT(t *testing.T) {
	srt := "1\n00:00:01,000 --> 00:00:02,000\nHello & World\n\n"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, srt)
	}))
	defer ts.Close()

	p := &prober{}
	var buf strings.Builder
	if err := p.WriteSubtitles(&buf, ts.URL+"/sub.srt", "srt", 0); err != nil {
		t.Fatalf("WriteSubtitles: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "00:00:01,000 --> 00:00:02,000") {
		t.Errorf("SRT output missing timestamp:\n%s", out)
	}
	// Ampersands must be escaped as &amp;
	if !strings.Contains(out, "Hello &amp; World") {
		t.Errorf("SRT output: ampersand not escaped:\n%s", out)
	}
}

func TestWriteSubtitlesVTT(t *testing.T) {
	srt := "1\n00:00:02,000 --> 00:00:03,500\nVTT cue\n\n"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, srt)
	}))
	defer ts.Close()

	p := &prober{}
	var buf strings.Builder
	if err := p.WriteSubtitles(&buf, ts.URL+"/sub.srt", "vtt", 0); err != nil {
		t.Fatalf("WriteSubtitles VTT: %v", err)
	}

	out := buf.String()
	if !strings.HasPrefix(out, "WEBVTT") {
		t.Errorf("VTT output must start with WEBVTT:\n%s", out)
	}
	// VTT uses dot separator, not comma.
	if !strings.Contains(out, "00:00:02.000 --> 00:00:03.500") {
		t.Errorf("VTT timestamp format wrong:\n%s", out)
	}
}

func TestWriteSubtitlesOffset(t *testing.T) {
	// 500ms offset added to both timestamps.
	srt := "1\n00:00:01,000 --> 00:00:02,000\nOffset test\n\n"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, srt)
	}))
	defer ts.Close()

	p := &prober{}
	var buf strings.Builder
	if err := p.WriteSubtitles(&buf, ts.URL+"/sub.srt", "srt", 500); err != nil {
		t.Fatalf("WriteSubtitles offset: %v", err)
	}

	out := buf.String()
	// 1000 + 500 = 1500ms, 2000 + 500 = 2500ms
	if !strings.Contains(out, "00:00:01,500 --> 00:00:02,500") {
		t.Errorf("offset not applied correctly:\n%s", out)
	}
}

func TestWriteSubtitlesNegativeOffset(t *testing.T) {
	// Negative offset clamped to 0.
	srt := "1\n00:00:00,500 --> 00:00:01,000\nNeg test\n\n"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, srt)
	}))
	defer ts.Close()

	p := &prober{}
	var buf strings.Builder
	if err := p.WriteSubtitles(&buf, ts.URL+"/sub.srt", "srt", -1000); err != nil {
		t.Fatalf("WriteSubtitles negative offset: %v", err)
	}

	out := buf.String()
	// 500 - 1000 = -500 → clamped to 0; 1000 - 1000 = 0
	if !strings.Contains(out, "00:00:00,000 --> 00:00:00,000") {
		t.Errorf("negative offset not clamped to 0:\n%s", out)
	}
}

// ── Extras: strconv helpers used in WriteSubtitles (errWriter) ───────────────

func TestErrWriterStickyError(t *testing.T) {
	// errWriter must not overwrite the first error.
	ew := &errWriter{w: &failWriter{}, err: nil}
	ew.printf("test %d", 1)
	if ew.err == nil {
		t.Fatal("errWriter should have captured write error")
	}
	first := ew.err
	ew.printf("another write") // must no-op after first error
	if !errors.Is(ew.err, first) {
		t.Error("errWriter replaced error on second write")
	}
}

// failWriter always returns an error on Write.
type failWriter struct{}

func (f *failWriter) Write(_ []byte) (int, error) {
	return 0, fmt.Errorf("write failed")
}

// ── Extras: strconv helpers sanity ───────────────────────────────────────────

func TestFtoaBasic(t *testing.T) {
	got := ftoa(4.123456789)
	// strconv.FormatFloat with 'f', prec=3 → "4.123"
	want := strconv.FormatFloat(4.123456789, 'f', 3, 64)
	if got != want {
		t.Errorf("ftoa mismatch: got %q, want %q", got, want)
	}
}
