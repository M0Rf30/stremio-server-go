// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package streamproxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

type postCall struct {
	method string
	url    string
	hdr    map[string]string
	body   string
}

// recordingGet answers every request with resp and records it.
func recordingGet(resp string, calls *[]postCall) pageFetcher {
	return func(_ *http.Request, method, rawurl string, hdr map[string]string, body []byte) (string, error) {
		*calls = append(*calls, postCall{method, rawurl, hdr, string(body)})
		return resp, nil
	}
}

const trickyValue = `he said "hi" \ <b>&`

func TestDefPostBodyTemplatingAndEscaping(t *testing.T) {
	ds := mustDefs(t, `{"version":1,"extractors":{"p":{
	  "steps":[
	    {"value":"he said \"hi\" \\ <b>&","set":"v"},
	    {"fetch":"{origin}/api","method":"POST",
	     "body":{"q":"{v}","n":5,"big":12345678901234567890,"nest":{"l":["x-{v}",true,null,{"{v}":"{host}"}]},"opt":"{missing?yes}{v?!}"},
	     "json":"0.url","set":"src"}
	  ],
	  "result":{"url":"{src}","endpoint":"hls"}}}}`)
	var calls []postCall
	r := httptest.NewRequest("GET", "/", nil)
	res, err := ds.Extractors["p"].run(r, "https://site.example/p/1", recordingGet(`[{"url":"https://cdn.example/a.m3u8"}]`, &calls))
	if err != nil {
		t.Fatal(err)
	}
	if res.URL != "https://cdn.example/a.m3u8" || res.Endpoint != "/proxy/hls/manifest.m3u8" {
		t.Errorf("result %+v", res)
	}
	if len(calls) != 1 || calls[0].method != "POST" || calls[0].url != "https://site.example/api" {
		t.Fatalf("calls %+v", calls)
	}
	if ct := calls[0].hdr["Content-Type"]; ct != "application/json" {
		t.Errorf("content-type %q", ct)
	}
	// The body is valid JSON and the substituted value round-trips exactly.
	var got struct {
		Q    string `json:"q"`
		N    int    `json:"n"`
		Big  json.Number
		Nest struct {
			L []any `json:"l"`
		} `json:"nest"`
		Opt string `json:"opt"`
	}
	dec := json.NewDecoder(strings.NewReader(calls[0].body))
	dec.UseNumber()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("body not valid JSON: %v\n%s", err, calls[0].body)
	}
	if got.Q != trickyValue || got.N != 5 || got.Opt != "!" {
		t.Errorf("decoded %+v\nraw %s", got, calls[0].body)
	}
	if !strings.Contains(calls[0].body, `"big":12345678901234567890`) {
		t.Errorf("number mangled: %s", calls[0].body)
	}
	if len(got.Nest.L) != 4 || got.Nest.L[0] != "x-"+trickyValue || got.Nest.L[1] != true || got.Nest.L[2] != nil {
		t.Errorf("nest %v", got.Nest.L)
	}
	// Object keys stay literal (not expanded).
	if m, _ := got.Nest.L[3].(map[string]any); m["{v}"] != "site.example" {
		t.Errorf("key expanded or value wrong: %v", got.Nest.L[3])
	}
	if !strings.Contains(calls[0].body, `\"hi\"`) {
		t.Errorf("quotes not escaped: %s", calls[0].body)
	}
}

func TestDefPostContentTypeOverrideAndNoBody(t *testing.T) {
	ds := mustDefs(t, `{"version":1,"extractors":{"p":{
	  "steps":[
	    {"fetch":"https://a.example/1","method":"post","headers":{"content-type":"application/vnd.x+json"},"body":{"a":"b"}},
	    {"fetch":"https://a.example/2","method":"POST"},
	    {"fetch":"https://a.example/3","method":"GET"}
	  ],
	  "result":{"url":"https://c.example/v.mp4","endpoint":"stream"}}}}`)
	var calls []postCall
	if _, err := ds.Extractors["p"].run(httptest.NewRequest("GET", "/", nil), "https://a.example/", recordingGet("x", &calls)); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 {
		t.Fatalf("calls %+v", calls)
	}
	if calls[0].method != "POST" || calls[0].hdr["content-type"] != "application/vnd.x+json" || calls[0].hdr["Content-Type"] != "" || calls[0].body != `{"a":"b"}` {
		t.Errorf("override call %+v", calls[0])
	}
	if calls[1].method != "POST" || calls[1].body != "" || calls[1].hdr["Content-Type"] != "" {
		t.Errorf("bodyless POST %+v", calls[1])
	}
	if calls[2].method != "GET" || calls[2].body != "" {
		t.Errorf("GET %+v", calls[2])
	}
}

func TestDefPostCountsAgainstFetchCapAndBodyCap(t *testing.T) {
	steps := make([]string, maxDefFetches+1)
	for i := range steps {
		steps[i] = `{"fetch":"https://a.example/","method":"POST","body":{"a":"b"}}`
	}
	ds := mustDefs(t, `{"version":1,"extractors":{"x":{"steps":[`+strings.Join(steps, ",")+`],"result":{"url":"{input}","endpoint":"stream"}}}}`)
	var calls []postCall
	_, err := ds.Extractors["x"].run(httptest.NewRequest("GET", "/", nil), "https://a.example/", recordingGet("x", &calls))
	if err == nil || !strings.Contains(err.Error(), "too many fetches") || len(calls) != maxDefFetches {
		t.Errorf("fetch cap: err=%v calls=%d", err, len(calls))
	}

	ds = mustDefs(t, `{"version":1,"extractors":{"x":{"steps":[
	  {"fetch":"https://a.example/page"},
	  {"fetch":"https://a.example/api","method":"POST","body":{"a":"{body}"}}
	],"result":{"url":"{input}","endpoint":"stream"}}}}`)
	calls = nil
	_, err = ds.Extractors["x"].run(httptest.NewRequest("GET", "/", nil), "https://a.example/", recordingGet(strings.Repeat("z", maxDefBody+1), &calls))
	if err == nil || !strings.Contains(err.Error(), "request body too large") {
		t.Errorf("body cap: %v", err)
	}
}

func TestParseDefSetPostAccepted(t *testing.T) {
	for _, body := range []string{`null`, `"s"`, `1`, `[]`, `{}`, `{"a":[1,{"b":"{x}"}]}`} {
		doc := `{"version":1,"extractors":{"a":{"steps":[{"fetch":"{input}","method":"POST","body":` + body + `}],"result":{"url":"{input}","endpoint":"hls"}}}}`
		if _, err := ParseDefSet([]byte(doc)); err != nil {
			t.Errorf("body %s rejected: %v", body, err)
		}
	}
}

const postDefs = `{"version":1,"extractors":{
  "post":{
    "match":"/post-iptv/play/[0-9]+",
    "steps":[
      {"value":"he said \"hi\" \\ end","set":"v"},
      {"fetch":"{origin}/api/resolve","method":"POST","headers":{"X-Api":"k"},
       "body":{"url":"{input}","note":"{v}"},"json":"0.url","set":"src"}
    ],
    "result":{"url":"{src}","headers":{"Referer":"https://post.example/","X-From-Def":"def"},"endpoint":"hls"}},
  "mpdhost":{
    "match":"/mpd-embed/",
    "steps":[{"fetch":"{input}"},{"regex":"file:\\s*\"([^\"]+)\"","set":"src"}],
    "result":{"url":"{src}","endpoint":"mpd"}}
}}`

type postUpstream struct {
	srv       *httptest.Server
	posts     atomic.Int32
	postBody  atomic.Value // string
	postCT    atomic.Value // string
	playHdrs  atomic.Value // http.Header of the last playlist request
	plainHits atomic.Int32
}

func newPostUpstream(t *testing.T) *postUpstream {
	t.Helper()
	u := &postUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/resolve" && r.Method == http.MethodPost:
			u.posts.Add(1)
			b, _ := io.ReadAll(r.Body)
			u.postBody.Store(string(b))
			u.postCT.Store(r.Header.Get("Content-Type"))
			_, _ = w.Write([]byte(`[{"url":"` + u.srv.URL + `/live/index.m3u8"}]`))
		case r.URL.Path == "/live/index.m3u8":
			u.playHdrs.Store(r.Header.Clone())
			_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nseg1.ts\n"))
		case r.URL.Path == "/plain.m3u8":
			u.plainHits.Add(1)
			_, _ = w.Write([]byte("#EXTM3U\n#EXTINF:4,\nplain1.ts\n"))
		case strings.HasPrefix(r.URL.Path, "/mpd-embed/"):
			_, _ = w.Write([]byte(`file: "` + u.srv.URL + `/live/index.m3u8"`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func newPostHandler(t *testing.T) *Handler {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "extractors.json"), []byte(postDefs), 0o600); err != nil {
		t.Fatal(err)
	}
	h := New(Config{AppPath: dir, PublicURL: "https://ext.example"})
	t.Cleanup(h.Close)
	return h
}

func proxyReq(path, dest, extra string) *http.Request {
	return httptest.NewRequest("GET", path+"?d="+url.QueryEscape(dest)+extra, nil)
}

func TestProxyHLSResolvesMatchingDest(t *testing.T) {
	up := newPostUpstream(t)
	h := newPostHandler(t)
	page := up.srv.URL + "/post-iptv/play/12345"

	w := httptest.NewRecorder()
	h.Route(w, proxyReq("/proxy/hls/manifest.m3u8", page, ""), []string{"proxy", "hls"})
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Body.String(), "#EXTM3U") || !strings.Contains(w.Body.String(), "/proxy/stream/segment.ts?d=") {
		t.Fatalf("hls: %d %q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "https://ext.example/proxy/") {
		t.Errorf("playlist not rewritten through the proxy: %q", w.Body.String())
	}
	if got, _ := up.postCT.Load().(string); got != "application/json" {
		t.Errorf("POST content-type %q", got)
	}
	var sent struct{ URL, Note string }
	body, _ := up.postBody.Load().(string)
	if err := json.Unmarshal([]byte(body), &sent); err != nil || sent.URL != page || sent.Note != `he said "hi" \ end` {
		t.Errorf("POST body %q (%v) %+v", body, err, sent)
	}
	hdr, _ := up.playHdrs.Load().(http.Header)
	if hdr.Get("Referer") != "https://post.example/" || hdr.Get("X-From-Def") != "def" {
		t.Errorf("result headers not sent upstream: %v", hdr)
	}

	// Cached for subsequent requests: one POST in total.
	w = httptest.NewRecorder()
	h.Route(w, proxyReq("/proxy/hls/manifest.m3u8", page, ""), []string{"proxy", "hls"})
	if w.Code != http.StatusOK || up.posts.Load() != 1 {
		t.Errorf("second: %d posts=%d", w.Code, up.posts.Load())
	}

	// Caller h_ headers win over the definition's.
	w = httptest.NewRecorder()
	h.Route(w, proxyReq("/proxy/hls/manifest.m3u8", page, "&h_X-From-Def=caller"), []string{"proxy", "hls"})
	if hdr, _ := up.playHdrs.Load().(http.Header); w.Code != http.StatusOK || hdr.Get("X-From-Def") != "caller" || hdr.Get("Referer") != "https://post.example/" {
		t.Errorf("caller header: %d %v", w.Code, hdr)
	}
}

func TestProxyHLSNonMatchingUntouched(t *testing.T) {
	up := newPostUpstream(t)
	h := newPostHandler(t)
	w := httptest.NewRecorder()
	h.Route(w, proxyReq("/proxy/hls/manifest.m3u8", up.srv.URL+"/plain.m3u8", ""), []string{"proxy", "hls"})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "/proxy/stream/segment.ts?d=") || up.posts.Load() != 0 || up.plainHits.Load() != 1 {
		t.Errorf("non-matching altered: %d %q posts=%d hits=%d", w.Code, w.Body.String(), up.posts.Load(), up.plainHits.Load())
	}
}

func TestProxyStreamMatchRedirectsToHLSEndpoint(t *testing.T) {
	up := newPostUpstream(t)
	h := newPostHandler(t)
	w := httptest.NewRecorder()
	h.serveStream(w, streamReq(up.srv.URL+"/post-iptv/play/9", "", nil))
	loc := w.Header().Get("Location")
	if w.Code != http.StatusFound || !strings.HasPrefix(loc, "https://ext.example/proxy/hls/manifest.m3u8?d=") {
		t.Fatalf("want 302 to hls, got %d %s", w.Code, loc)
	}
}

func TestProxyMPDMatchRedirectsToResultEndpoint(t *testing.T) {
	// A definition whose result endpoint differs from the requested one is
	// served through its own endpoint (here mpd -> mpd in place, hls -> mpd
	// by redirect).
	up := newPostUpstream(t)
	h := newPostHandler(t)

	w := httptest.NewRecorder()
	h.Route(w, proxyReq("/proxy/hls/manifest.m3u8", up.srv.URL+"/mpd-embed/1", ""), []string{"proxy", "hls"})
	loc := w.Header().Get("Location")
	if w.Code != http.StatusFound || !strings.HasPrefix(loc, "https://ext.example/proxy/mpd/manifest.m3u8?d=") {
		t.Fatalf("want 302 to mpd, got %d %s", w.Code, loc)
	}
	u, _ := url.Parse(loc)
	if decodeDest(u.Query().Get("d")) != up.srv.URL+"/live/index.m3u8" {
		t.Errorf("redirect target %s", decodeDest(u.Query().Get("d")))
	}
}

func TestProxyHLSMatchFailure(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "captcha", http.StatusForbidden)
	}))
	defer bad.Close()
	h := newPostHandler(t)
	w := httptest.NewRecorder()
	h.Route(w, proxyReq("/proxy/hls/manifest.m3u8", bad.URL+"/post-iptv/play/1", ""), []string{"proxy", "hls"})
	if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "captcha") {
		t.Errorf("unresolvable: %d %q", w.Code, w.Body.String())
	}
}
