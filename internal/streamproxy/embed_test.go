// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package streamproxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const embedDefs = `{"version":1,"extractors":{
  "embedhost":{
    "match":"/e/[a-z0-9]+",
    "steps":[{"fetch":"{input}"},{"regex":"src:\\s*\"([^\"]+)\"","set":"src"}],
    "result":{"url":"{src}","headers":{"Referer":"{input}","X-From-Def":"def"},"endpoint":"stream"}},
  "embedhls":{
    "match":"/hls-embed/",
    "steps":[{"fetch":"{input}"},{"regex":"file:\\s*\"([^\"]+)\"","set":"src"}],
    "result":{"url":"{src}","endpoint":"hls"}}
}}`

type embedUpstream struct {
	srv       *httptest.Server
	pageHits  atomic.Int32
	mediaHdrs atomic.Value // http.Header of the last media request
}

func newEmbedUpstream(t *testing.T) *embedUpstream {
	t.Helper()
	u := &embedUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/e/"):
			u.pageHits.Add(1)
			_, _ = w.Write([]byte(`<script>player.setup({src: "` + u.srv.URL + `/media/v.mp4"})</script>`))
		case strings.HasPrefix(r.URL.Path, "/hls-embed/"):
			_, _ = w.Write([]byte(`<script>file: "` + u.srv.URL + `/live/index.m3u8"</script>`))
		case r.URL.Path == "/media/v.mp4":
			u.mediaHdrs.Store(r.Header.Clone())
			http.ServeContent(w, r, "v.mp4", time.Time{}, strings.NewReader("0123456789MEDIA"))
		case r.URL.Path == "/plain.mp4":
			_, _ = w.Write([]byte("PLAIN"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func newEmbedHandler(t *testing.T) *Handler {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "extractors.json"), []byte(embedDefs), 0o600); err != nil {
		t.Fatal(err)
	}
	h := New(Config{AppPath: dir, PublicURL: "https://ext.example"})
	t.Cleanup(h.Close)
	return h
}

func streamReq(dest string, extra string, hdr map[string]string) *http.Request {
	r := httptest.NewRequest("GET", "/proxy/stream?d="+url.QueryEscape(dest)+extra, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

func TestProxyStreamResolvesEmbedPage(t *testing.T) {
	up := newEmbedUpstream(t)
	h := newEmbedHandler(t)
	page := up.srv.URL + "/e/abc123"

	w := httptest.NewRecorder()
	h.serveStream(w, streamReq(page, "", nil))
	if w.Code != http.StatusOK || w.Body.String() != "0123456789MEDIA" {
		t.Fatalf("full: %d %q", w.Code, w.Body.String())
	}
	got := up.mediaHdrs.Load().(http.Header)
	if got.Get("Referer") != page || got.Get("X-From-Def") != "def" {
		t.Errorf("definition headers not sent upstream: %v", got)
	}

	// Range requests reuse the cached resolution (one page fetch in total).
	for range 3 {
		w = httptest.NewRecorder()
		h.serveStream(w, streamReq(page, "", map[string]string{"Range": "bytes=10-"}))
		if w.Code != http.StatusPartialContent || w.Body.String() != "MEDIA" {
			t.Fatalf("range: %d %q", w.Code, w.Body.String())
		}
	}
	if n := up.pageHits.Load(); n != 1 {
		t.Errorf("embed page fetched %d times, want 1 (cache)", n)
	}

	// Caller-supplied h_ headers win over the definition's.
	w = httptest.NewRecorder()
	h.serveStream(w, streamReq(page, "&h_X-From-Def=caller", nil))
	if got := up.mediaHdrs.Load().(http.Header); got.Get("X-From-Def") != "caller" {
		t.Errorf("caller header overridden: %v", got.Get("X-From-Def"))
	}
}

func TestProxyStreamEmbedRedirectsPlaylists(t *testing.T) {
	up := newEmbedUpstream(t)
	h := newEmbedHandler(t)
	w := httptest.NewRecorder()
	h.serveStream(w, streamReq(up.srv.URL+"/hls-embed/1", "", nil))
	loc := w.Header().Get("Location")
	if w.Code != http.StatusFound || !strings.HasPrefix(loc, "https://ext.example/proxy/hls/manifest.m3u8?d=") {
		t.Fatalf("want 302 to hls endpoint, got %d %s", w.Code, loc)
	}
	u, _ := url.Parse(loc)
	if decodeDest(u.Query().Get("d")) != up.srv.URL+"/live/index.m3u8" {
		t.Errorf("redirect target %s", decodeDest(u.Query().Get("d")))
	}
}

func TestProxyStreamNonMatchingUntouched(t *testing.T) {
	up := newEmbedUpstream(t)
	h := newEmbedHandler(t)
	w := httptest.NewRecorder()
	h.serveStream(w, streamReq(up.srv.URL+"/plain.mp4", "", nil))
	if w.Code != http.StatusOK || w.Body.String() != "PLAIN" || up.pageHits.Load() != 0 {
		t.Errorf("non-matching dest altered: %d %q hits=%d", w.Code, w.Body.String(), up.pageHits.Load())
	}
}

func TestProxyStreamEmbedFailure(t *testing.T) {
	h := newEmbedHandler(t)
	// A matching page without the expected script yields 502, not the HTML.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>captcha</html>"))
	}))
	defer bad.Close()
	w := httptest.NewRecorder()
	h.serveStream(w, streamReq(bad.URL+"/e/nope", "", nil))
	if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "captcha") {
		t.Errorf("unresolvable embed: %d %q", w.Code, w.Body.String())
	}
}

func TestEmbedCacheBounded(t *testing.T) {
	var c embedCache
	for i := range embedCacheMax + 50 {
		c.put(string(rune('a'+i%26))+strings.Repeat("x", i), &extractResult{URL: "u"})
	}
	if len(c.m) > embedCacheMax {
		t.Errorf("cache grew to %d", len(c.m))
	}
}

func TestParseDefSetBadMatch(t *testing.T) {
	if _, err := ParseDefSet([]byte(`{"version":1,"extractors":{"a":{"match":"(","steps":[{"fetch":"{input}"}],"result":{"url":"{input}","endpoint":"stream"}}}}`)); err == nil {
		t.Error("invalid match regex accepted")
	}
}
