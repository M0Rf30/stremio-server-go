// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package streamproxy

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fakeGet(pages map[string]string) pageFetcher {
	return func(_ *http.Request, _, rawurl string, _ map[string]string, _ []byte) (string, error) {
		if b, ok := pages[rawurl]; ok {
			return b, nil
		}
		return "", errExtract
	}
}

// playlistDefs is a fixture definition: optional JSON API hop, optional
// iframe hop, then token/expires/playlist captures.
const playlistDefs = `{
  "version": 1,
  "extractors": {
    "Example": {
      "steps": [
        {"if_path": "^/(movie|tv)/", "fetch": "{origin}/api{path}", "json": "src", "set": "embed", "optional": true},
        {"fetch": "{embed}", "headers": {"Referer": "{input}"}, "optional": true},
        {"if_url": "/iframe", "regex": "<iframe[^>]+src=[\"']([^\"']+)", "transform": ["html_unescape"], "follow": true},
        {"regex": "'token'\\s*:\\s*'(\\w+)'", "set": "token"},
        {"regex": "'expires'\\s*:\\s*'(\\d+)'", "set": "expires"},
        {"regex": "url\\s*:\\s*'(https?://[^']+)'", "set": "playlist"},
        {"regex": "window\\.canPlayFHD\\s*=\\s*true", "set": "fhd", "optional": true}
      ],
      "result": {
        "url": "{playlist}",
        "query": {"token": "{token}", "expires": "{expires}", "h": "{fhd?1}"},
        "headers": {"Referer": "{page_origin}/", "Origin": "{page_origin}"},
        "endpoint": "hls"
      }
    }
  }
}`

const playerHTML = `<script>window.masterPlaylist = { params: { 'token': 'abcDEF123', 'expires': '1760000000', },
	url: 'https://cdn.example/playlist/999?b=1', }
	window.canPlayFHD = true</script>`

func mustDefs(t *testing.T, doc string) *DefSet {
	t.Helper()
	ds, err := ParseDefSet([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

func TestDefRunAPIHop(t *testing.T) {
	ex := mustDefs(t, playlistDefs).Extractors["example"]
	r := httptest.NewRequest("GET", "/", nil)
	res, err := ex.run(r, "https://player.example/movie/123", fakeGet(map[string]string{
		"https://player.example/api/movie/123":     `{"src":"/embed/777?lang=it"}`,
		"https://player.example/embed/777?lang=it": playerHTML,
	}))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(res.URL)
	q := u.Query()
	if u.Host != "cdn.example" || u.Path != "/playlist/999" || q.Get("token") != "abcDEF123" ||
		q.Get("expires") != "1760000000" || q.Get("h") != "1" || q.Get("b") != "1" {
		t.Errorf("bad url %s", res.URL)
	}
	if res.Endpoint != "/proxy/hls/manifest.m3u8" || res.Headers["Referer"] != "https://player.example/" ||
		res.Headers["Origin"] != "https://player.example" {
		t.Errorf("bad result %+v", res)
	}
}

func TestDefRunIframeAndOptional(t *testing.T) {
	ex := mustDefs(t, playlistDefs).Extractors["example"]
	r := httptest.NewRequest("GET", "/", nil)
	noFHD := strings.Replace(playerHTML, "window.canPlayFHD = true", "", 1)
	res, err := ex.run(r, "https://site.example/iframe/5", fakeGet(map[string]string{
		"https://site.example/iframe/5":         `<iframe src="https://other.example/embed/5?a=1&amp;b=2"></iframe>`,
		"https://other.example/embed/5?a=1&b=2": noFHD,
	}))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(res.URL)
	if u.Query().Has("h") || res.Headers["Origin"] != "https://other.example" {
		t.Errorf("bad result %+v", res)
	}
	if _, err := ex.run(r, "https://site.example/iframe/5", fakeGet(map[string]string{
		"https://site.example/iframe/5": "<html></html>",
	})); err == nil {
		t.Error("expected failure without iframe/playlist")
	}
}

func TestDefRunPackedJS(t *testing.T) {
	ds := mustDefs(t, `{"version":1,"extractors":{"packed":{
	  "steps":[
	    {"fetch":"{input}","transform":["unpack_js"]},
	    {"regex":"V\\.src\\s*=\\s*\"([^\"]+)\"","transform":["scheme_https"],"set":"src"}],
	  "result":{"url":"{src}","headers":{"Referer":"{input}"},"endpoint":"stream"}}}}`)
	packed := `<script>eval(function(p,a,c,k,e,d){}('0.1="//2.3/4.5";',10,6,'V|src|cdn1|example|v|mp4'.split('|'),0,{}))</script>`
	r := httptest.NewRequest("GET", "/", nil)
	res, err := ds.Extractors["packed"].run(r, "https://h.example/e/abc", fakeGet(map[string]string{"https://h.example/e/abc": packed}))
	if err != nil {
		t.Fatal(err)
	}
	if res.URL != "https://cdn1.example/v.mp4" || res.Endpoint != "/proxy/stream" || res.Headers["Referer"] != "https://h.example/e/abc" {
		t.Errorf("bad result %+v", res)
	}
}

func TestDefFetchCap(t *testing.T) {
	steps := make([]string, maxDefFetches+1)
	for i := range steps {
		steps[i] = `{"fetch":"{input}"}`
	}
	ds := mustDefs(t, `{"version":1,"extractors":{"x":{"steps":[`+strings.Join(steps, ",")+`],"result":{"url":"{input}","endpoint":"stream"}}}}`)
	r := httptest.NewRequest("GET", "/", nil)
	_, err := ds.Extractors["x"].run(r, "https://a.example/", fakeGet(map[string]string{"https://a.example/": "x"}))
	if err == nil || !strings.Contains(err.Error(), "too many fetches") {
		t.Errorf("got %v", err)
	}
}

func TestParseDefSetRejects(t *testing.T) {
	for name, doc := range map[string]string{
		"version":   `{"version":2,"extractors":{}}`,
		"unknown":   `{"version":1,"extractors":{},"x":1}`,
		"endpoint":  `{"version":1,"extractors":{"a":{"steps":[{"fetch":"{input}"}],"result":{"url":"{input}","endpoint":"ftp"}}}}`,
		"regex":     `{"version":1,"extractors":{"a":{"steps":[{"regex":"("}],"result":{"url":"{input}","endpoint":"hls"}}}}`,
		"transform": `{"version":1,"extractors":{"a":{"steps":[{"fetch":"{input}","transform":["eval"]}],"result":{"url":"{input}","endpoint":"hls"}}}}`,
		"noaction":  `{"version":1,"extractors":{"a":{"steps":[{"optional":true}],"result":{"url":"{input}","endpoint":"hls"}}}}`,
		"follow":    `{"version":1,"extractors":{"a":{"steps":[{"fetch":"{input}","follow":true}],"result":{"url":"{input}","endpoint":"hls"}}}}`,
		"nourl":     `{"version":1,"extractors":{"a":{"steps":[{"fetch":"{input}"}],"result":{"endpoint":"hls"}}}}`,
		"badmethod": `{"version":1,"extractors":{"a":{"steps":[{"fetch":"{input}","method":"PUT"}],"result":{"url":"{input}","endpoint":"hls"}}}}`,
		"bodyget":   `{"version":1,"extractors":{"a":{"steps":[{"fetch":"{input}","body":{"a":1}}],"result":{"url":"{input}","endpoint":"hls"}}}}`,
		"bodyget2":  `{"version":1,"extractors":{"a":{"steps":[{"fetch":"{input}","method":"GET","body":"x"}],"result":{"url":"{input}","endpoint":"hls"}}}}`,
		"nofetch":   `{"version":1,"extractors":{"a":{"steps":[{"value":"x","method":"POST"}],"result":{"url":"{input}","endpoint":"hls"}}}}`,
		"badbody":   `{"version":1,"extractors":{"a":{"steps":[{"fetch":"{input}","method":"POST","body":{"a":}}],"result":{"url":"{input}","endpoint":"hls"}}}}`,
		"deepbody":  `{"version":1,"extractors":{"a":{"steps":[{"fetch":"{input}","method":"POST","body":` + strings.Repeat("[", 40) + strings.Repeat("]", 40) + `}],"result":{"url":"{input}","endpoint":"hls"}}}}`,
	} {
		if _, err := ParseDefSet([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestExpandAndJSONPath(t *testing.T) {
	st := &defRun{vars: map[string]string{"a": "x", "e": ""}}
	if got := st.expand("{a}/{e?yes}{a?yes}{missing}"); got != "x/yes" {
		t.Errorf("expand = %q", got)
	}
	var doc any
	_ = json.Unmarshal([]byte(`{"a":[{"b":"c"},{"n":2.5,"t":true}]}`), &doc)
	for path, want := range map[string]string{"a.0.b": "c", "a.1.n": "2.5", "a.1.t": "true"} {
		if got, ok := jsonPath(doc, path); !ok || got != want {
			t.Errorf("jsonPath(%s) = %q,%v", path, got, ok)
		}
	}
	if _, ok := jsonPath(doc, "a.9"); ok {
		t.Error("out of range index accepted")
	}
}

func TestParseBase(t *testing.T) {
	for _, tc := range []struct {
		s     string
		radix int
		want  int
	}{{"a", 36, 10}, {"10", 62, 62}, {"Z", 62, 61}, {"9", 10, 9}} {
		if got, ok := parseBase(tc.s, tc.radix); !ok || got != tc.want {
			t.Errorf("parseBase(%q,%d)=%d,%v want %d", tc.s, tc.radix, got, ok, tc.want)
		}
	}
	if _, ok := parseBase("z", 10); ok {
		t.Error("digit out of radix accepted")
	}
}

func TestRegistryLocalReload(t *testing.T) {
	dir := t.TempDir()
	reg := newDefRegistry(Config{AppPath: dir})
	defer reg.close()
	if _, ok := reg.lookup("example"); ok {
		t.Fatal("found definition with no file")
	}
	path := filepath.Join(dir, "extractors.json")
	if err := os.WriteFile(path, []byte(playlistDefs), 0o600); err != nil {
		t.Fatal(err)
	}
	reg.lastCheck.Store(0)
	if _, ok := reg.lookup("EXAMPLE"); !ok {
		t.Fatal("definition not loaded")
	}
	// An invalid edit keeps the previous good set.
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(path, time.Now().Add(time.Minute), time.Now().Add(time.Minute))
	reg.lastCheck.Store(0)
	if _, ok := reg.lookup("example"); !ok {
		t.Fatal("previous set dropped after invalid edit")
	}
	_ = os.Remove(path)
	reg.lastCheck.Store(0)
	if _, ok := reg.lookup("example"); ok {
		t.Fatal("definition kept after file removal")
	}
}

// TestRegistryConcurrentReload: readers run lock-free while the local file is
// rewritten and reloaded; under -race this exercises the snapshot handoff.
func TestRegistryConcurrentReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "extractors.json")
	write := func(match string) {
		doc := `{"version":1,"extractors":{"e":{"match":"` + match + `","steps":[{"fetch":"{input}"}],"result":{"url":"{input}","endpoint":"stream"}}}}`
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Error(err)
		}
	}
	write("/a/")
	reg := newDefRegistry(Config{AppPath: dir})
	defer reg.close()
	if _, _, ok := reg.match("https://h/a/1"); !ok {
		t.Fatal("initial definitions not loaded eagerly")
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 8 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					_, _, _ = reg.match("https://h/a/1")
					_, _ = reg.lookup("e")
				}
			}
		})
	}
	for i := range 20 {
		write([]string{"/a/", "/b/"}[i%2])
		_ = os.Chtimes(path, time.Now().Add(time.Duration(i+1)*time.Second), time.Now().Add(time.Duration(i+1)*time.Second))
		reg.lastCheck.Store(0)
		_, _, _ = reg.match("x")
	}
	close(stop)
	wg.Wait()
	// Last write was "/b/" (i=19).
	reg.lastCheck.Store(0)
	if _, _, ok := reg.match("https://h/b/1"); !ok {
		t.Error("final reload not visible")
	}
}

func TestRegistryRemoteSignature(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	doc := []byte(playlistDefs)
	goodSig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, doc))
	sig := goodSig
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sig") {
			_, _ = w.Write([]byte(sig))
			return
		}
		_, _ = w.Write(doc)
	}))
	defer srv.Close()

	dir := t.TempDir()
	reg := &defRegistry{remoteURL: srv.URL + "/defs.json", pubKey: pub, cachePath: filepath.Join(dir, remoteDefCacheName), stop: make(chan struct{})}
	if err := reg.fetchRemote(); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.lookup("example"); !ok {
		t.Fatal("signed remote set not loaded")
	}
	// Cached copy is reloaded and verified on startup.
	reg2 := &defRegistry{pubKey: pub, cachePath: reg.cachePath, stop: make(chan struct{})}
	reg2.loadRemoteCache()
	if _, ok := reg2.lookup("example"); !ok {
		t.Fatal("cached remote set not loaded")
	}

	sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte("other")))
	reg3 := &defRegistry{remoteURL: srv.URL + "/defs.json", pubKey: pub, stop: make(chan struct{})}
	if err := reg3.fetchRemote(); err == nil {
		t.Fatal("bad signature accepted")
	}
	if _, ok := reg3.lookup("example"); ok {
		t.Fatal("unsigned set served")
	}
}

func TestRemoteRequiresPubKey(t *testing.T) {
	reg := newDefRegistry(Config{ExtractorsURL: "https://defs.example/x.json"})
	defer reg.close()
	if reg.remoteURL != "" {
		t.Error("remote enabled without a public key")
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	for _, enc := range []string{hex.EncodeToString(pub), base64.StdEncoding.EncodeToString(pub)} {
		if _, err := parsePubKey(enc); err != nil {
			t.Errorf("parsePubKey(%s): %v", enc, err)
		}
	}
	if _, err := parsePubKey("abcd"); err == nil {
		t.Error("short key accepted")
	}
}

func TestHandleExtractorRedirectAndJSON(t *testing.T) {
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Replace(playerHTML, "https://cdn.example", upstream.URL, 1)))
	}))
	defer upstream.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "extractors.json"), []byte(playlistDefs), 0o600); err != nil {
		t.Fatal(err)
	}
	h := New(Config{AppPath: dir})
	defer h.Close()
	page := url.QueryEscape(upstream.URL + "/embed/1")

	w := httptest.NewRecorder()
	h.HandleExtractor(w, httptest.NewRequest("GET", "/extractor/video?host=Example&d="+page, nil), []string{"extractor", "video"})
	if w.Code != http.StatusOK {
		t.Fatalf("json: %d %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["mediaflow_endpoint"] != "hls_manifest_proxy" ||
		!strings.Contains(body["destination_url"].(string), "token=abcDEF123") {
		t.Errorf("bad json %s", w.Body.String())
	}

	w = httptest.NewRecorder()
	h.HandleExtractor(w, httptest.NewRequest("GET", "/extractor/video.m3u8?host=example&redirect_stream=true&d="+page, nil), []string{"extractor", "video.m3u8"})
	if w.Code != http.StatusFound || !strings.Contains(w.Header().Get("Location"), "/proxy/hls/manifest.m3u8?d=") {
		t.Errorf("redirect: %d %s", w.Code, w.Header().Get("Location"))
	}
}

func TestHandleExtractorErrors(t *testing.T) {
	h := New(Config{Password: "pw"})
	defer h.Close()
	for _, tc := range []struct {
		path string
		seg  []string
		code int
	}{
		{"/extractor/video?host=Example&d=https://x.example/e/1", []string{"extractor", "video"}, http.StatusUnauthorized},
		{"/extractor/nope?api_password=pw", []string{"extractor", "nope"}, http.StatusNotFound},
		{"/extractor/video.m3u8?host=Unknown&d=https://x.example/&api_password=pw", []string{"extractor", "video.m3u8"}, http.StatusBadRequest},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", tc.path, nil)
		r.RemoteAddr = "203.0.113.1:1234"
		h.HandleExtractor(w, r, tc.seg)
		if w.Code != tc.code {
			t.Errorf("%s: code %d want %d (%s)", tc.path, w.Code, tc.code, w.Body.String())
		}
	}
}
