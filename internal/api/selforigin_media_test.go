// SPDX-FileCopyrightText: 2026 The rivulet-engine Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	lzstring "github.com/daku10/go-lz-string"

	"github.com/rivulet-kodi/rivulet-engine/internal/media"
	"github.com/rivulet-kodi/rivulet-engine/internal/types"
)

// pathRecorder records every path an httptest handler is asked for.
type pathRecorder struct {
	mu   sync.Mutex
	hits []string
}

func (p *pathRecorder) record(path string) {
	p.mu.Lock()
	p.hits = append(p.hits, path)
	p.mu.Unlock()
}

func (p *pathRecorder) paths() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.hits...)
}

func (p *pathRecorder) hit(path string) bool { return slices.Contains(p.paths(), path) }

func (p *pathRecorder) hitPrefix(prefix string) bool {
	return slices.ContainsFunc(p.paths(), func(s string) bool { return strings.HasPrefix(s, prefix) })
}

// selfOriginServer runs the real handler with the real media prober on a
// loopback listener whose URL is the prober's selfBase, exactly as
// internal/app wires them, and records every path the server is asked for.
type selfOriginServer struct {
	*httptest.Server
	pathRecorder
	em *fakeEM
}

func newSelfOriginServer(t *testing.T) *selfOriginServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + ln.Addr().String()
	prober := media.New(base, media.HLSConfig{WorkDir: t.TempDir()}, nil)
	if c, ok := prober.(interface{ CloseHLS() }); ok {
		t.Cleanup(c.CloseHLS)
	}
	s := &selfOriginServer{em: newFakeEM(testEngine())}
	cfg := types.Config{
		HTTPPort:           11470,
		WebUI:              "https://web.stremio.com/",
		CreateMetadataWait: 5 * time.Second,
		AppPath:            t.TempDir(),
		SelfURL:            base,
	}
	h := New(s.em, &fakeSS{}, prober, cfg)
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.record(r.URL.Path)
		h.ServeHTTP(w, r)
	}))
	_ = s.Listener.Close()
	s.Listener = ln
	s.Start()
	t.Cleanup(s.Close)
	return s
}

func (s *selfOriginServer) crossSiteMaster(t *testing.T, id, mediaURL string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.URL+"/hlsv2/"+id+"/master.m3u8?mediaURL="+url.QueryEscape(mediaURL), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Sec-Fetch-Dest", "video")
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(body)
}

func requireFFprobe(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not on PATH")
	}
}

// TestCrossSiteMasterSelfRemoveAllRefused is issue #65: master.m3u8 is exempt
// from the cross-site check (<video src> loads it), so any web page could
// name a self-origin control route as the mediaURL and have ffprobe request
// it. The mediaURL must be refused (like any other rejected mediaURL, with
// StartHLS's error status) and the route never reached.
func TestCrossSiteMasterSelfRemoveAllRefused(t *testing.T) {
	s := newSelfOriginServer(t)
	for _, p := range []string{"/removeAll", "/removeAll/x.ts", "/extractor/x", "/ftp/stream", "/" + testIH + "/remove"} {
		code, body := s.crossSiteMaster(t, "x", s.URL+p)
		if code == http.StatusOK || !strings.Contains(body, "is not a media route") {
			t.Errorf("cross-site master.m3u8?mediaURL=<self>%s = %d %q; want it refused as a non-media self route", p, code, body)
		}
		if s.hit(p) {
			t.Errorf("mediaURL <self>%s was requested; a refused self path must never be fetched", p)
		}
	}
	if engines := s.em.ListEngines(); len(engines) != 1 {
		t.Errorf("engines after the requests = %v; RemoveAll must not have run", engines)
	}
}

// TestCrossSiteMasterSelfTorrentFileAllowed: a torrent file on the server's
// own origin is local content and stays a valid mediaURL; ffprobe really
// reads it (the fake file is not media, so the transcode itself then fails).
func TestCrossSiteMasterSelfTorrentFileAllowed(t *testing.T) {
	requireFFprobe(t)
	s := newSelfOriginServer(t)
	p := "/" + testIH + "/0"
	if code, body := s.crossSiteMaster(t, "x", s.URL+p); strings.Contains(body, "is not a media route") {
		t.Errorf("cross-site master.m3u8?mediaURL=<self>%s = %d %q; a torrent file must stay allowed", p, code, body)
	}
	if !s.hit(p) {
		t.Errorf("mediaURL <self>%s was never requested; ffprobe should have read it", p)
	}
}

// TestCrossSiteMasterSelfArchiveCreateAllowed: stremio-core's playable URL
// for an archive stream is GET /{ext}/create?lz=…, which redirects to
// /{ext}/stream/{key}/{file}. It stays a valid mediaURL, and ffprobe
// follows it through to the archive entry.
func TestCrossSiteMasterSelfArchiveCreateAllowed(t *testing.T) {
	requireFFprobe(t)
	zp := writeTestZip(t, "movie.mkv", []byte(strings.Repeat("not media ", 1000)))
	t.Setenv("STREMIO_ARCHIVE_LOCAL_ROOT", filepath.Dir(zp))
	lz, err := lzstring.CompressToEncodedURIComponent(fmt.Sprintf(`{"url":%q}`, zp))
	if err != nil {
		t.Fatal(err)
	}
	s := newSelfOriginServer(t)
	mediaURL := s.URL + "/zip/create?lz=" + url.QueryEscape(lz)
	if code, body := s.crossSiteMaster(t, "zipsess", mediaURL); strings.Contains(body, "is not a media route") {
		t.Fatalf("cross-site master.m3u8?mediaURL=<self>/zip/create = %d %q; it must stay allowed", code, body)
	}
	if !s.hit("/zip/create") {
		t.Errorf("/zip/create was never requested; hits %v", s.paths())
	}
	if !s.hitPrefix("/zip/stream/") {
		t.Errorf("ffprobe did not follow /zip/create to /zip/stream/…; hits %v", s.paths())
	}
}

// TestCrossSiteMasterSelfProxyRelayed: stremio-core wraps a stream with
// proxyHeaders as /proxy/d=<origin>&h=…/{path}. That self URL is read
// through the media relay: the proxied playlist is fetched (through /proxy,
// so streamproxy's own guard applies), its relative entries resolve onto
// self /proxy and are fetched the same way, and its hostile entries (self
// /removeAll and a private host named directly) are never requested.
func TestCrossSiteMasterSelfProxyRelayed(t *testing.T) {
	requireFFprobe(t)
	// The proxy's destination is loopback here, so let streamproxy reach it;
	// the relay's own guard is unaffected.
	t.Setenv("STREMIO_PROXY_ALLOW_PRIVATE", "1")
	s := newSelfOriginServer(t)

	var private pathRecorder
	ps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		private.record(r.URL.Path)
		_, _ = io.WriteString(w, "PRIVATE")
	}))
	t.Cleanup(ps.Close)

	var origin pathRecorder
	osrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin.record(r.URL.Path)
		switch r.URL.Path {
		case "/v/hostile.m3u8":
			// Hostile entries first, with a media extension (FFmpeg 8's
			// extension_picky); /removeAll/x.ts still routes to /removeAll.
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n"+
				"#EXTINF:4,\n%s/private.ts\n"+
				"#EXTINF:4,\n%s/removeAll/x.ts\n"+
				"#EXT-X-ENDLIST\n", ps.URL, s.URL)
		case "/v/good.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nseg0.ts\n#EXT-X-ENDLIST\n")
		default:
			_, _ = io.WriteString(w, strings.Repeat("not media ", 100))
		}
	}))
	t.Cleanup(osrv.Close)
	proxyURL := func(path string) string {
		// As stremio-core builds it: the form-encoded options are the
		// path segment as is (streamproxy reads it from the wire path).
		q := url.Values{"d": {osrv.URL}, "h": {"User-Agent:test"}}.Encode()
		return s.URL + "/proxy/" + q + path
	}

	if code, body := s.crossSiteMaster(t, "proxy-good", proxyURL("/v/good.m3u8")); strings.Contains(body, "is not a media route") {
		t.Fatalf("cross-site master.m3u8?mediaURL=<self>/proxy/… = %d %q; it must stay allowed", code, body)
	}
	if !origin.hit("/v/good.m3u8") || !origin.hit("/v/seg0.ts") {
		t.Errorf("proxied playlist and its relative segment not both fetched through /proxy; origin hits %v", origin.paths())
	}

	s.crossSiteMaster(t, "proxy-hostile", proxyURL("/v/hostile.m3u8"))
	if !origin.hit("/v/hostile.m3u8") {
		t.Fatalf("hostile playlist never fetched; origin hits %v", origin.paths())
	}
	if s.hit("/removeAll/x.ts") {
		t.Error("a nested playlist entry reached self /removeAll")
	}
	if hits := private.paths(); len(hits) != 0 {
		t.Errorf("a nested playlist entry reached the private host directly: %v", hits)
	}
	if engines := s.em.ListEngines(); len(engines) != 1 {
		t.Errorf("engines after the requests = %v; RemoveAll must not have run", engines)
	}
}
