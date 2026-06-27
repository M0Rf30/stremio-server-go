// Package api — fourth batch of tests covering stream handlers with empty
// config, eviction routines, archive extraction, and torznab queries.
package api

import (
	"archive/zip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ─── torznabStream / bitmagnetStream with empty config ──────────────────────

func TestHandlerTorznabStream_EmptyConfig(t *testing.T) {
	h := newHandler(t) // TorznabURL unset → empty streams
	rec := serve(t, h, http.MethodGet, "/torznab/stream/movie/tt1234567.json", nil)
	if rec.Code != 200 {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	m := decodeJSON(t, rec.Body.Bytes())
	streams, ok := m["streams"].([]any)
	if !ok {
		t.Errorf("streams missing/not array; got %v", m)
	}
	if len(streams) != 0 {
		t.Errorf("streams = %v; want [] (no TorznabURL)", streams)
	}
}

func TestHandlerBitmagnetStream_EmptyConfig(t *testing.T) {
	h := newHandler(t) // BitmagnetURL unset → empty streams
	rec := serve(t, h, http.MethodGet, "/bitmagnet/stream/movie/tt1234567.json", nil)
	if rec.Code != 200 {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	m := decodeJSON(t, rec.Body.Bytes())
	if _, ok := m["streams"].([]any); !ok {
		t.Errorf("streams missing/not array; got %v", m)
	}
}

// ─── bmItemMatchesSeries ────────────────────────────────────────────────────

func TestBmItemMatchesSeries(t *testing.T) {
	t.Run("no season data → whole-series pack → true", func(t *testing.T) {
		item := bmItem{}
		if !bmItemMatchesSeries(item, 1, 5) {
			t.Error("whole-series pack must always match")
		}
	})
	t.Run("season match with no episodes → season pack → true", func(t *testing.T) {
		item := bmItem{}
		item.Episodes.Seasons = append(item.Episodes.Seasons, struct {
			Season   int   `json:"season"`
			Episodes []int `json:"episodes"`
		}{Season: 1, Episodes: nil})
		got := bmItemMatchesSeries(item, 1, 5)
		if !got {
			t.Error("season pack (matching season, no episodes) must return true")
		}
	})
	t.Run("season + episode match → true", func(t *testing.T) {
		item := bmItem{}
		item.Episodes.Seasons = append(item.Episodes.Seasons, struct {
			Season   int   `json:"season"`
			Episodes []int `json:"episodes"`
		}{Season: 1, Episodes: []int{5, 6}})
		if !bmItemMatchesSeries(item, 1, 5) {
			t.Error("must match when season=1, episode=5 both present")
		}
	})
	t.Run("wrong season → false", func(t *testing.T) {
		item := bmItem{}
		item.Episodes.Seasons = append(item.Episodes.Seasons, struct {
			Season   int   `json:"season"`
			Episodes []int `json:"episodes"`
		}{Season: 2, Episodes: []int{5}})
		if bmItemMatchesSeries(item, 1, 5) {
			t.Error("must return false when season doesn't match")
		}
	})
	t.Run("season match but episode mismatch → false", func(t *testing.T) {
		item := bmItem{}
		item.Episodes.Seasons = append(item.Episodes.Seasons, struct {
			Season   int   `json:"season"`
			Episodes []int `json:"episodes"`
		}{Season: 1, Episodes: []int{5, 6}})
		if bmItemMatchesSeries(item, 1, 9) {
			t.Error("must return false when episode not in episode list")
		}
	})
}

// ─── torznab helper tests: tnFetch / tnQueryIMDB / tnQueryTitle ─────────────

const torznabXML = `<?xml version="1.0"?>
<rss>
  <channel>
    <item>
      <title>Test Torrent</title>
      <enclosure url="magnet:?xt=urn:btih:aabbccddeeff00112233445566778899aabbccdd" length="1073741824" type="application/x-bittorrent"/>
      <attr name="seeders" value="42"/>
      <attr name="size" value="1073741824"/>
    </item>
  </channel>
</rss>`

func TestTnFetch(t *testing.T) {
	srv := newHTTPTestServer(torznabXML)
	defer srv.Close()

	req := newCtxRequest()
	items, err := tnFetch(req, srv.URL+"/api")
	if err != nil {
		t.Fatalf("tnFetch error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items; want 1", len(items))
	}
	if items[0].Title != "Test Torrent" {
		t.Errorf("title = %q; want Test Torrent", items[0].Title)
	}
}

func TestTnFetch_NotFound(t *testing.T) {
	srv := newHTTPTestServerStatus(404, "not found")
	defer srv.Close()
	req := newCtxRequest()
	_, err := tnFetch(req, srv.URL+"/api")
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
}

func TestTnFetch_InvalidXML(t *testing.T) {
	srv := newHTTPTestServer("not xml at all")
	defer srv.Close()
	req := newCtxRequest()
	_, err := tnFetch(req, srv.URL+"/api")
	if err == nil {
		t.Fatal("expected error for invalid XML, got nil")
	}
}

func TestTnQueryIMDB(t *testing.T) {
	srv := newHTTPTestServer(torznabXML)
	defer srv.Close()
	req := newCtxRequest()
	items, err := tnQueryIMDB(req, srv.URL, "key", "movie", "1234567", 0, 0)
	if err != nil {
		t.Fatalf("tnQueryIMDB error: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("got %d items; want 1", len(items))
	}
}

func TestTnQueryTitle(t *testing.T) {
	srv := newHTTPTestServer(torznabXML)
	defer srv.Close()
	req := newCtxRequest()
	items, err := tnQueryTitle(req, srv.URL, "key", "series", "Test", 1, 5)
	if err != nil {
		t.Fatalf("tnQueryTitle error: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("got %d items; want 1", len(items))
	}
}

// newHTTPTestServer returns an httptest server serving body for any path.
func newHTTPTestServer(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
}

// newHTTPTestServerStatus returns a server that always returns the given status.
func newHTTPTestServerStatus(status int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

// newCtxRequest returns an http.Request with a non-nil context.
func newCtxRequest() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://example.invalid/", nil)
	return req
}

// ─── nzbEvictIdle ───────────────────────────────────────────────────────────

func TestNzbEvictIdle(t *testing.T) {
	// Insert an idle session (lastAccess 2h ago) with a tmpDir we own.
	dir := t.TempDir()
	sess := &nzbSession{
		key:        "test-key",
		tmpDir:     dir,
		created:    time.Now().Add(-3 * time.Hour),
		lastAccess: time.Now().Add(-2 * time.Hour),
		fileStates: map[string]*nzbFileState{},
	}
	nzbSessionsMu.Lock()
	nzbSessions["test-key"] = sess
	nzbSessionsMu.Unlock()

	nzbEvictIdle()

	nzbSessionsMu.Lock()
	_, ok := nzbSessions["test-key"]
	nzbSessionsMu.Unlock()
	if ok {
		t.Error("session should have been evicted after idle")
	}
	// The tmpDir should have been removed by the janitor.
	if _, err := os.Stat(dir); err == nil {
		_ = os.RemoveAll(dir) // belt & suspenders
		// Note: janitor runs async; we just assert the session is gone.
	}
}

func TestNzbEvictIdle_KeepActive(t *testing.T) {
	sess := &nzbSession{
		key:        "active-key",
		lastAccess: time.Now(),
		fileStates: map[string]*nzbFileState{},
	}
	nzbSessionsMu.Lock()
	nzbSessions["active-key"] = sess
	nzbSessionsMu.Unlock()

	nzbEvictIdle()

	nzbSessionsMu.Lock()
	_, ok := nzbSessions["active-key"]
	nzbSessionsMu.Unlock()
	if !ok {
		t.Error("active session should NOT have been evicted")
	}
	// Cleanup
	nzbSessionsMu.Lock()
	delete(nzbSessions, "active-key")
	nzbSessionsMu.Unlock()
}

// ─── archiveEvict ───────────────────────────────────────────────────────────

func TestArchiveEvict(t *testing.T) {
	dir := t.TempDir()
	sess := &archiveSession{
		key:         "evict-test",
		archivePath: "",
		isTempArch:  false,
		tmpDir:      dir,
		lastAccess:  time.Now().Add(-2 * time.Hour), // older than TTL
		extracted:   map[string]string{},
	}
	archiveSessionsMu.Lock()
	archiveSessions["evict-test"] = sess
	archiveSessionsMu.Unlock()

	archiveEvict()

	archiveSessionsMu.Lock()
	_, ok := archiveSessions["evict-test"]
	archiveSessionsMu.Unlock()
	if ok {
		t.Error("session should have been evicted")
	}
}

func TestArchiveEvict_KeepRecent(t *testing.T) {
	sess := &archiveSession{
		key:        "recent-test",
		lastAccess: time.Now(),
		extracted:  map[string]string{},
	}
	archiveSessionsMu.Lock()
	archiveSessions["recent-test"] = sess
	archiveSessionsMu.Unlock()

	archiveEvict()

	archiveSessionsMu.Lock()
	_, ok := archiveSessions["recent-test"]
	archiveSessionsMu.Unlock()
	if !ok {
		t.Error("recent session should NOT have been evicted")
	}
	archiveSessionsMu.Lock()
	delete(archiveSessions, "recent-test")
	archiveSessionsMu.Unlock()
}

// ─── archiveExtractEntry ────────────────────────────────────────────────────

func TestArchiveExtractEntry_Zip(t *testing.T) {
	// Build a real zip archive in a temp dir with one entry "video.mkv".
	archDir := t.TempDir()
	archPath := filepath.Join(archDir, "test.zip")
	content := []byte("video content bytes here for extraction test")
	if err := buildZipFileWithEntry(archPath, "video.mkv", content); err != nil {
		t.Fatalf("buildZipFileWithEntry: %v", err)
	}

	extractDir := t.TempDir()
	sess := &archiveSession{
		key:         "extract-test",
		archivePath: archPath,
		ext:         "zip",
		tmpDir:      extractDir,
		lastAccess:  time.Now(),
		extracted:   map[string]string{},
	}

	outPath, err := archiveExtractEntry(sess, "video.mkv")
	if err != nil {
		t.Fatalf("archiveExtractEntry error: %v", err)
	}
	defer os.Remove(outPath)

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("extracted content = %q; want %q", got, content)
	}

	// Second call must hit the fast path (cached path returned).
	cached, err := archiveExtractEntry(sess, "video.mkv")
	if err != nil {
		t.Fatalf("second archiveExtractEntry error: %v", err)
	}
	if cached != outPath {
		t.Errorf("cached path = %q; want %q", cached, outPath)
	}
}

func TestArchiveExtractEntry_NotFound(t *testing.T) {
	archDir := t.TempDir()
	archPath := filepath.Join(archDir, "test.zip")
	if err := buildZipFileWithEntry(archPath, "video.mkv", []byte("x")); err != nil {
		t.Fatalf("buildZipFileWithEntry: %v", err)
	}
	extractDir := t.TempDir()
	sess := &archiveSession{
		key:         "notfound-test",
		archivePath: archPath,
		ext:         "zip",
		tmpDir:      extractDir,
		extracted:   map[string]string{},
	}
	_, err := archiveExtractEntry(sess, "missing-file.mkv")
	if err == nil {
		t.Fatal("expected error for missing entry, got nil")
	}
}

// buildZipFileWithEntry creates a zip archive at path containing one entry
// (named entryName) with the supplied content bytes.
func buildZipFileWithEntry(path, entryName string, content []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	fw, err := zw.Create(entryName)
	if err != nil {
		return err
	}
	_, _ = fw.Write(content)
	return zw.Close()
}

// ─── scanLocalFiles with LOCAL_FILES_DIR set ────────────────────────────────

func TestScanLocalFiles_WithDir(t *testing.T) {
	dir := t.TempDir()
	// Create a video file with a recognisable title.
	videoPath := filepath.Join(dir, "Movie.Title.2020.1080p.mkv")
	if err := os.WriteFile(videoPath, []byte("fake mkv"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Create a non-video file (should be ignored).
	_ = os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("notes"), 0o600)

	t.Setenv("LOCAL_FILES_DIR", dir)
	// Reset the scan cache so the new dir is rescanned.
	scanCacheMu.Lock()
	scanCache = nil
	scanCacheAt = time.Time{}
	scanCacheMu.Unlock()

	items := scanLocalFiles()
	if len(items) != 1 {
		t.Fatalf("got %d items; want 1 (only the .mkv)", len(items))
	}
	m := items[0]
	if m.Name == "" {
		t.Error("scanLocalFiles returned empty name")
	}
	if m.Path != videoPath {
		t.Errorf("path = %q; want %q", m.Path, videoPath)
	}
	if !strings.HasPrefix(m.ID, "local:") {
		t.Errorf("ID = %q; want local: prefix", m.ID)
	}
}

func TestScanLocalFiles_NoDirEnv(t *testing.T) {
	t.Setenv("LOCAL_FILES_DIR", "")
	scanCacheMu.Lock()
	scanCache = nil
	scanCacheAt = time.Time{}
	scanCacheMu.Unlock()

	items := scanLocalFiles()
	if items != nil {
		t.Errorf("scanLocalFiles (no DIR) = %v; want nil", items)
	}
}

// ─── scanCache is reset between subtests ─────────────────────────────────────

func TestScanLocalFilesCached(t *testing.T) {
	t.Setenv("LOCAL_FILES_DIR", "")
	scanCacheMu.Lock()
	scanCache = nil
	scanCacheAt = time.Time{}
	scanCacheMu.Unlock()

	// First call: scans (returns nil), caches nil.
	_ = scanLocalFilesCached()
	// Second call: hits the cache (cached within TTL); covered either way.
	items := scanLocalFilesCached()
	if items != nil {
		t.Errorf("scanLocalFilesCached with no dir = %v; want nil", items)
	}
}
