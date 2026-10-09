// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/dvr"
	"github.com/M0Rf30/stremio-server-go/internal/types"
)

const (
	recSrc = "http://93.184.216.34/live/index.m3u8"
	recPW  = "s3cret"
)

// fakeRec stands in for ffmpeg: it creates the output file with some bytes,
// then runs until it is asked to quit.
type fakeRec struct {
	noFFmpeg bool
	mu       sync.Mutex
	runs     [][]string
	procs    []*fakeRecProc
	data     string
}

type fakeRecProc struct {
	out  string
	quit chan struct{}
	once sync.Once
	n    int
}

func (p *fakeRecProc) Wait() error    { <-p.quit; return nil }
func (p *fakeRecProc) Quit()          { p.n++; p.once.Do(func() { close(p.quit) }) }
func (p *fakeRecProc) Kill()          { p.once.Do(func() { close(p.quit) }) }
func (p *fakeRecProc) Output() string { return "" }

func (f *fakeRec) start(_ string, args []string) (dvr.Proc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := args[len(args)-1]
	if err := os.WriteFile(out, []byte(f.data), 0o600); err != nil {
		return nil, err
	}
	p := &fakeRecProc{out: out, quit: make(chan struct{})}
	f.runs = append(f.runs, args)
	f.procs = append(f.procs, p)
	return p, nil
}

func (f *fakeRec) arg(i int, flag string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.runs[i]
	for j := 0; j+1 < len(a); j++ {
		if a[j] == flag {
			return a[j+1]
		}
	}
	return ""
}

func (f *fakeRec) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

// dvrHandler builds a handler with DVR enabled and ffmpeg faked.
func dvrHandler(t *testing.T, fake *fakeRec, mutate func(*types.Config)) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	dvrStartFunc = fake.start
	dvrLookPath = func(string) (string, error) {
		if fake.noFFmpeg {
			return "", errors.New("executable file not found in $PATH")
		}
		return "/fake/ffmpeg", nil
	}
	t.Cleanup(func() { dvrStartFunc, dvrLookPath = nil, nil })
	h := newHandlerWithCfg(t, func(c *types.Config) {
		c.DVREnabled = true
		c.DVRDir = filepath.Join(dir, "rec")
		c.DVRDefaultDuration = 4 * time.Hour
		c.DVRMaxDuration = 8 * time.Hour
		c.DVRMaxActive = 2
		c.ProxyPassword = recPW
		if mutate != nil {
			mutate(c)
		}
	})
	t.Cleanup(func() {
		if c, ok := h.(io.Closer); ok {
			_ = c.Close()
		}
	})
	return h, filepath.Join(dir, "rec")
}

func recQuery(src, extra string) string {
	return "/record?url=" + url.QueryEscape(src) + "&api_password=" + recPW + extra
}

func do(h http.Handler, method, target string, hdr map[string]string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func startRec(t *testing.T, h http.Handler, extra string) (id string, rec *httptest.ResponseRecorder) {
	t.Helper()
	rec = do(h, http.MethodGet, recQuery(recSrc, extra), nil, nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("/record = %d %s", rec.Code, rec.Body.String())
	}
	id = rec.Header().Get("X-Recording-Id")
	if !dvr.ValidID(id) {
		t.Fatalf("X-Recording-Id = %q", id)
	}
	return id, rec
}

func TestDVRDisabledByDefault(t *testing.T) {
	h := newHandler(t)
	for _, p := range []string{"/record?url=http://x/y.m3u8", "/api/recordings", "/api/recordings/active", "/record/stop/x"} {
		rec := do(h, http.MethodGet, p, nil, nil)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "DVR is disabled") {
			t.Errorf("%s = %d %s; want 404 DVR is disabled", p, rec.Code, rec.Body.String())
		}
	}
}

func TestDVRRequiresPassword(t *testing.T) {
	fake := &fakeRec{data: "x"}
	h, _ := dvrHandler(t, fake, nil)
	src := url.QueryEscape(recSrc)
	for _, c := range []struct {
		name, method, target string
		hdr                  map[string]string
		want                 int
	}{
		{"record none", "GET", "/record?url=" + src, nil, 401},
		{"record wrong", "GET", "/record?url=" + src + "&api_password=nope", nil, 401},
		{"list none", "GET", "/api/recordings", nil, 401},
		{"list wrong", "GET", "/api/recordings?api_password=x", nil, 401},
		{"active none", "GET", "/api/recordings/active", nil, 401},
		{"start none", "POST", "/api/recordings/start", nil, 401},
		{"detail none", "GET", "/api/recordings/20260101_000000_aaaaaaaa", nil, 401},
		{"stop none", "POST", "/api/recordings/20260101_000000_aaaaaaaa/stop", nil, 401},
		{"delete none", "DELETE", "/api/recordings/20260101_000000_aaaaaaaa", nil, 401},
		{"delete-get none", "GET", "/api/recordings/20260101_000000_aaaaaaaa/delete", nil, 401},
		{"stream none", "GET", "/api/recordings/20260101_000000_aaaaaaaa/stream", nil, 401},
		{"download none", "GET", "/api/recordings/20260101_000000_aaaaaaaa/download", nil, 401},
		{"stop-and-watch none", "GET", "/record/stop/20260101_000000_aaaaaaaa", nil, 401},
		{"header wrong", "GET", "/api/recordings", map[string]string{"X-Api-Password": "bad"}, 401},
		{"query ok", "GET", "/api/recordings?api_password=" + recPW, nil, 200},
		{"header ok", "GET", "/api/recordings", map[string]string{"X-Api-Password": recPW}, 200},
	} {
		rec := do(h, c.method, c.target, c.hdr, nil)
		if rec.Code != c.want {
			t.Errorf("%s: %d %s; want %d", c.name, rec.Code, rec.Body.String(), c.want)
		}
	}
	if fake.count() != 0 {
		t.Errorf("unauthenticated requests spawned %d recorders", fake.count())
	}
}

func TestDVRIPACLForbidden(t *testing.T) {
	fake := &fakeRec{data: "x"}
	h, _ := dvrHandler(t, fake, func(c *types.Config) { c.ProxyIPACL = "10.0.0.0/8" })
	req := httptest.NewRequest(http.MethodGet, recQuery(recSrc, ""), nil)
	req.RemoteAddr = "192.0.2.9:1111"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || fake.count() != 0 {
		t.Errorf("ACL-denied /record = %d (runs=%d)", rec.Code, fake.count())
	}
}

func TestDVRNoPasswordConfiguredIsOpen(t *testing.T) {
	fake := &fakeRec{data: "x"}
	h, _ := dvrHandler(t, fake, func(c *types.Config) { c.ProxyPassword = "" })
	rec := do(h, http.MethodGet, "/record?url="+url.QueryEscape(recSrc), nil, nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("= %d %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "api_password") {
		t.Errorf("redirect carries a password that was never configured: %s", loc)
	}
}

func TestRecordStartsAndRedirectsToLiveProxy(t *testing.T) {
	fake := &fakeRec{data: "TSDATA"}
	h, dir := dvrHandler(t, fake, nil)
	id, rec := startRec(t, h, "&name="+url.QueryEscape("Sky Sport F1")+"&duration=600")

	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || loc.IsAbs() {
		t.Fatalf("Location should be relative to the proxy: %q", rec.Header().Get("Location"))
	}
	if loc.Path != "/proxy/hls/manifest.m3u8" || loc.Query().Get("api_password") != recPW {
		t.Errorf("Location = %s", loc)
	}
	if dec, err := base64.RawURLEncoding.DecodeString(loc.Query().Get("d")); err != nil || string(dec) != recSrc {
		t.Errorf("redirect d = %q (%v); want base64url(%s)", loc.Query().Get("d"), err, recSrc)
	}

	// ffmpeg reads the same proxy URL on loopback, copies, and is capped at 600 s.
	in := fake.arg(0, "-i")
	if !strings.HasPrefix(in, "http://127.0.0.1:11470/proxy/hls/manifest.m3u8?d=") || !strings.Contains(in, "api_password="+recPW) {
		t.Errorf("ffmpeg input = %s", in)
	}
	if fake.arg(0, "-t") != "600" || fake.arg(0, "-c") != "copy" || fake.arg(0, "-f") != "mpegts" {
		t.Errorf("ffmpeg args = %v", fake.runs[0])
	}
	out := fake.runs[0][len(fake.runs[0])-1]
	if filepath.Dir(out) != dir || !strings.HasSuffix(out, "_Sky_Sport_F1.ts") {
		t.Errorf("output = %s", out)
	}

	// A player retrying the URL does not start a second recorder.
	if rec2 := do(h, http.MethodGet, recQuery(recSrc, ""), nil, nil); rec2.Code != http.StatusFound || rec2.Header().Get("X-Recording-Id") != id {
		t.Errorf("repeat /record = %d id=%s", rec2.Code, rec2.Header().Get("X-Recording-Id"))
	}
	if fake.count() != 1 {
		t.Errorf("repeat spawned %d recorders", fake.count())
	}
}

func TestRecordDurationClamp(t *testing.T) {
	fake := &fakeRec{data: "x"}
	h, _ := dvrHandler(t, fake, func(c *types.Config) {
		c.DVRDefaultDuration = 2 * time.Hour
		c.DVRMaxDuration = 3 * time.Hour
		c.DVRMaxActive = 10
	})
	for i, c := range []struct{ q, want string }{
		{"", "7200"},                          // default
		{"&duration=", "7200"},                // empty -> default
		{"&duration=0", "7200"},               // zero -> default
		{"&duration=-30", "7200"},             // negative -> default
		{"&duration=90", "90"},                // honoured
		{"&duration=14400", "10800"},          // a 4h request clamped to the 3h cap
		{"&duration=99999999999999", "10800"}, // absurd -> cap, no overflow
	} {
		src := recSrc + "?n=" + string(rune('a'+i))
		rec := do(h, http.MethodGet, recQuery(src, c.q), nil, nil)
		if rec.Code != http.StatusFound {
			t.Fatalf("%q: %d %s", c.q, rec.Code, rec.Body.String())
		}
		if got := fake.arg(i, "-t"); got != c.want {
			t.Errorf("duration%q -> -t %s; want %s", c.q, got, c.want)
		}
	}
	for _, bad := range []string{"&duration=abc", "&duration=1.5", "&duration=1h"} {
		rec := do(h, http.MethodGet, recQuery(recSrc+"?z", bad), nil, nil)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Duration must be a number") {
			t.Errorf("%s = %d %s", bad, rec.Code, rec.Body.String())
		}
	}
}

func TestRecordNameCannotEscapeDirectory(t *testing.T) {
	fake := &fakeRec{data: "x"}
	h, dir := dvrHandler(t, fake, func(c *types.Config) { c.DVRMaxActive = 10 })
	for i, name := range []string{"../../../../etc/passwd", "/abs/evil", `..\..\evil`, "%00x", "../"} {
		rec := do(h, http.MethodGet, recQuery(recSrc+"?i="+string(rune('a'+i)), "&name="+url.QueryEscape(name)), nil, nil)
		if rec.Code != http.StatusFound {
			t.Fatalf("name %q: %d", name, rec.Code)
		}
		out := fake.runs[i][len(fake.runs[i])-1]
		if filepath.Dir(out) != dir {
			t.Errorf("name %q -> %s escapes %s", name, out, dir)
		}
	}
	ents, _ := os.ReadDir(filepath.Dir(dir))
	if len(ents) != 1 {
		t.Errorf("stray entries beside the recordings dir: %v", ents)
	}
	// And the listing never exposes a path.
	list := decodeJSON(t, do(h, http.MethodGet, "/api/recordings?api_password="+recPW, nil, nil).Body.Bytes())
	for _, r := range list["recordings"].([]any) {
		fp := r.(map[string]any)["file_path"].(string)
		if strings.ContainsAny(fp, `/\`) {
			t.Errorf("file_path %q is not a bare name", fp)
		}
	}
}

func TestRecordValidation(t *testing.T) {
	fake := &fakeRec{data: "x"}
	h, _ := dvrHandler(t, fake, nil)
	for _, c := range []struct {
		name, target string
		want         int
	}{
		{"missing url", "/record?api_password=" + recPW, 400},
		{"file scheme", recQuery("file:///etc/passwd", ""), 400},
		{"loopback dest", recQuery("http://127.0.0.1:1/x.m3u8", ""), 400},
		{"metadata dest", recQuery("http://169.254.169.254/latest/", ""), 400},
		{"unknown extractor", recQuery(recSrc, "&extractor=nope"), 400},
		{"bad method", "", 405},
	} {
		method := "GET"
		if c.name == "bad method" {
			method, c.target = "POST", recQuery(recSrc, "")
		}
		rec := do(h, method, c.target, nil, nil)
		if rec.Code != c.want {
			t.Errorf("%s = %d %s; want %d", c.name, rec.Code, rec.Body.String(), c.want)
		}
	}
	if fake.count() != 0 {
		t.Errorf("rejected requests spawned %d recorders", fake.count())
	}
}

func TestRecordWithoutFFmpegIs503(t *testing.T) {
	fake := &fakeRec{data: "x", noFFmpeg: true}
	h, dir := dvrHandler(t, fake, nil)
	rec := do(h, http.MethodGet, recQuery(recSrc, ""), nil, nil)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "ffmpeg not found") {
		t.Fatalf("= %d %s", rec.Code, rec.Body.String())
	}
	post := do(h, http.MethodPost, "/api/recordings/start?api_password="+recPW, nil, strings.NewReader(`{"url":"`+recSrc+`"}`))
	if post.Code != http.StatusServiceUnavailable {
		t.Errorf("POST start = %d", post.Code)
	}
	if fake.count() != 0 {
		t.Error("recorder spawned without ffmpeg")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("files created without ffmpeg: %v", ents)
	}
	// Listing/deleting still work (existing recordings stay manageable).
	if c := do(h, "GET", "/api/recordings?api_password="+recPW, nil, nil).Code; c != 200 {
		t.Errorf("list without ffmpeg = %d", c)
	}
}

func TestRecordBusyIs429(t *testing.T) {
	fake := &fakeRec{data: "x"}
	h, _ := dvrHandler(t, fake, func(c *types.Config) { c.DVRMaxActive = 1 })
	startRec(t, h, "")
	rec := do(h, http.MethodGet, recQuery(recSrc+"?other", ""), nil, nil)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Errorf("second recording = %d %s", rec.Code, rec.Body.String())
	}
}

func TestRecordingsListShape(t *testing.T) {
	fake := &fakeRec{data: "TSDATA"}
	h, _ := dvrHandler(t, fake, nil)
	id, _ := startRec(t, h, "&name=News")
	rec := do(h, http.MethodGet, "/api/recordings?api_password="+recPW, nil, nil)
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("list = %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := decodeJSON(t, rec.Body.Bytes())
	recs := body["recordings"].([]any)
	if len(recs) != 1 || body["active_count"].(float64) != 1 {
		t.Fatalf("list body = %v", body)
	}
	if _, ok := body["system_stats"].(map[string]any); !ok {
		t.Error("system_stats missing")
	}
	r0 := recs[0].(map[string]any)
	// Every field DVR addons read must be present.
	for _, k := range []string{"id", "name", "url", "file_path", "status", "started_at", "stopped_at",
		"duration_seconds", "file_size_bytes", "is_active", "elapsed_seconds"} {
		if _, ok := r0[k]; !ok {
			t.Errorf("recording lacks %q: %v", k, r0)
		}
	}
	if r0["id"] != id || r0["name"] != "News" || r0["status"] != "recording" || r0["is_active"] != true {
		t.Errorf("recording = %v", r0)
	}
	if r0["url"] != recSrc { // api_password is stripped, the source is otherwise verbatim
		t.Errorf("url = %v", r0["url"])
	}
	if _, err := time.Parse(time.RFC3339, r0["started_at"].(string)); err != nil {
		t.Errorf("started_at %v is not RFC3339", r0["started_at"])
	}
	if strings.Contains(rec.Body.String(), recPW) {
		t.Error("listing leaks the api_password")
	}

	// Filters and /active.
	if n := len(decodeJSON(t, do(h, "GET", "/api/recordings?status=completed&api_password="+recPW, nil, nil).Body.Bytes())["recordings"].([]any)); n != 0 {
		t.Errorf("status filter returned %d", n)
	}
	act := decodeJSON(t, do(h, "GET", "/api/recordings/active?api_password="+recPW, nil, nil).Body.Bytes())["recordings"].([]any)
	if len(act) != 1 {
		t.Errorf("/active = %d", len(act))
	}
	det := do(h, "GET", "/api/recordings/"+id+"?api_password="+recPW, nil, nil)
	if det.Code != 200 || decodeJSON(t, det.Body.Bytes())["id"] != id {
		t.Errorf("detail = %d %s", det.Code, det.Body.String())
	}
	for _, bad := range []string{"20260101_000000_aaaaaaaa", "..%2F..%2Fetc%2Fpasswd", "x"} {
		if c := do(h, "GET", "/api/recordings/"+bad+"?api_password="+recPW, nil, nil).Code; c != 404 {
			t.Errorf("detail %s = %d; want 404", bad, c)
		}
	}
	// Empty store still has the array shape addons do `data.recordings || []` on.
	h2, _ := dvrHandler(t, &fakeRec{}, nil)
	if b := do(h2, "GET", "/api/recordings?api_password="+recPW, nil, nil).Body.String(); !strings.Contains(b, `"recordings":[]`) {
		t.Errorf("empty list = %s", b)
	}
}

func TestRecordingStopStreamDownloadDelete(t *testing.T) {
	fake := &fakeRec{data: "0123456789"}
	h, dir := dvrHandler(t, fake, nil)
	id, _ := startRec(t, h, "&name=Match")
	pw := "?api_password=" + recPW

	// POST stop -> stopped recording JSON.
	stop := do(h, "POST", "/api/recordings/"+id+"/stop"+pw, nil, nil)
	if stop.Code != 200 {
		t.Fatalf("stop = %d %s", stop.Code, stop.Body.String())
	}
	sv := decodeJSON(t, stop.Body.Bytes())
	if sv["status"] != "stopped" || sv["is_active"] != false || sv["file_size_bytes"].(float64) != 10 || sv["stopped_at"] == nil {
		t.Errorf("stopped = %v", sv)
	}
	if fake.procs[0].n != 1 {
		t.Errorf("recorder quit %d times", fake.procs[0].n)
	}
	if c := do(h, "POST", "/api/recordings/"+id+"/stop"+pw, nil, nil).Code; c != 404 {
		t.Errorf("second stop = %d; want 404", c)
	}

	// Playback with Range (finished recording -> ServeContent).
	str := do(h, "GET", "/api/recordings/"+id+"/stream"+pw, map[string]string{"Range": "bytes=2-5"}, nil)
	if str.Code != http.StatusPartialContent || str.Body.String() != "2345" || str.Header().Get("Content-Range") != "bytes 2-5/10" ||
		str.Header().Get("Content-Type") != "video/mp2t" {
		t.Errorf("stream range = %d %q %v", str.Code, str.Body.String(), str.Header())
	}
	full := do(h, "GET", "/api/recordings/"+id+"/stream"+pw, nil, nil)
	if full.Code != 200 || full.Body.String() != "0123456789" || full.Header().Get("Accept-Ranges") != "bytes" {
		t.Errorf("stream full = %d %q", full.Code, full.Body.String())
	}
	dl := do(h, "GET", "/api/recordings/"+id+"/download"+pw, map[string]string{"Range": "bytes=8-"}, nil)
	if dl.Code != http.StatusPartialContent || dl.Body.String() != "89" ||
		!strings.HasPrefix(dl.Header().Get("Content-Disposition"), `attachment; filename="`+id+"_Match.ts") {
		t.Errorf("download = %d %q %v", dl.Code, dl.Body.String(), dl.Header())
	}

	// GET /record/stop/{id} on a finished recording redirects to its stream URL.
	ws := do(h, "GET", "/record/stop/"+id+pw, nil, nil)
	if ws.Code != http.StatusFound {
		t.Fatalf("stop&watch = %d %s", ws.Code, ws.Body.String())
	}
	wl, _ := url.Parse(ws.Header().Get("Location"))
	if !wl.IsAbs() || wl.Path != "/api/recordings/"+id+"/stream" || wl.Query().Get("api_password") != recPW {
		t.Errorf("stop&watch Location = %s", ws.Header().Get("Location"))
	}

	// Legacy GET delete (for players that can only open a URL).
	del := do(h, "GET", "/api/recordings/"+id+"/delete"+pw, nil, nil)
	if del.Code != 200 || !strings.HasPrefix(del.Header().Get("Content-Type"), "text/plain") || !strings.Contains(del.Body.String(), "deleted") {
		t.Errorf("GET delete = %d %q", del.Code, del.Body.String())
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("delete left %v", ents)
	}
	for _, p := range []string{"/api/recordings/" + id + pw, "/api/recordings/" + id + "/stream" + pw, "/record/stop/" + id + pw, "/api/recordings/" + id + "/delete" + pw} {
		if c := do(h, "GET", p, nil, nil).Code; c != 404 {
			t.Errorf("GET %s after delete = %d", p, c)
		}
	}
	if c := do(h, "DELETE", "/api/recordings/"+id+pw, nil, nil).Code; c != 404 {
		t.Errorf("DELETE after delete = %d", c)
	}
}

func TestRecordingDeleteVerbAndAll(t *testing.T) {
	fake := &fakeRec{data: "x"}
	h, _ := dvrHandler(t, fake, nil)
	pw := "?api_password=" + recPW
	a, _ := startRec(t, h, "")
	rec := do(h, "DELETE", "/api/recordings/"+a+pw, nil, nil)
	if rec.Code != 200 || decodeJSON(t, rec.Body.Bytes())["success"] != true {
		t.Fatalf("DELETE = %d %s", rec.Code, rec.Body.String())
	}
	if fake.procs[0].n != 1 {
		t.Error("deleting an active recording must stop it first")
	}
	startRec(t, h, "")
	do(h, "POST", "/api/recordings/start"+pw, nil, strings.NewReader(`{"url":"`+recSrc+`?second","name":"B","duration":60}`))
	for _, p := range []string{"/api/recordings/all", "/api/recordings"} {
		all := do(h, "DELETE", p+pw, nil, nil)
		if all.Code != 200 {
			t.Fatalf("DELETE %s = %d", p, all.Code)
		}
		if p == "/api/recordings/all" {
			if got := decodeJSON(t, all.Body.Bytes()); got["success"] != true || got["deleted"].(float64) != 2 {
				t.Errorf("delete all = %v", got)
			}
			startRec(t, h, "")
		}
	}
	if n := len(decodeJSON(t, do(h, "GET", "/api/recordings"+pw, nil, nil).Body.Bytes())["recordings"].([]any)); n != 0 {
		t.Errorf("%d recordings remain", n)
	}
}

func TestStartViaAPI(t *testing.T) {
	fake := &fakeRec{data: "x"}
	h, _ := dvrHandler(t, fake, func(c *types.Config) { c.DVRMaxDuration = time.Hour })
	pw := "?api_password=" + recPW
	post := func(body string) *httptest.ResponseRecorder {
		return do(h, "POST", "/api/recordings/start"+pw, map[string]string{"Content-Type": "application/json"}, strings.NewReader(body))
	}
	rec := post(`{"url":"` + recSrc + `","name":"Via API","duration":99999}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("start = %d %s", rec.Code, rec.Body.String())
	}
	got := decodeJSON(t, rec.Body.Bytes())
	if got["name"] != "Via API" || got["status"] != "recording" || fake.arg(0, "-t") != "3600" {
		t.Errorf("start result %v, -t %s", got, fake.arg(0, "-t"))
	}
	if again := post(`{"url":"` + recSrc + `"}`); again.Code != http.StatusOK || decodeJSON(t, again.Body.Bytes())["id"] != got["id"] {
		t.Errorf("duplicate start = %d %s", again.Code, again.Body.String())
	}
	if c := post(`{"url":"` + recSrc + `?s","duration":"120"}`).Code; c != http.StatusCreated || fake.arg(1, "-t") != "120" {
		t.Errorf("string duration: %d -t %s", c, fake.arg(1, "-t"))
	}
	for body, want := range map[string]int{
		`not json`: 400,
		`{}`:       400,
		`{"url":"` + recSrc + `?x","duration":"x"}`: 400,
		`{"url":"file:///etc/passwd"}`:              400,
	} {
		if c := post(body).Code; c != want {
			t.Errorf("body %q = %d; want %d", body, c, want)
		}
	}
}

func TestFollowActiveRecordingWhileItGrows(t *testing.T) {
	fake := &fakeRec{data: "AAAA"}
	h, dir := dvrHandler(t, fake, nil)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Get(srv.URL + recQuery(recSrc, "&name=Live"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	var id string
	recs := dvrListIDs(t, srv.URL)
	if len(recs) != 1 {
		t.Fatalf("recordings = %v", recs)
	}
	id = recs[0]

	live, err := http.Get(srv.URL + "/api/recordings/" + id + "/stream?api_password=" + recPW)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = live.Body.Close() }()
	if live.StatusCode != 200 || live.Header.Get("Accept-Ranges") != "none" || live.Header.Get("Content-Type") != "video/mp2t" {
		t.Fatalf("live = %d %v", live.StatusCode, live.Header)
	}
	first := make([]byte, 4)
	if _, err := io.ReadFull(live.Body, first); err != nil || string(first) != "AAAA" {
		t.Fatalf("first chunk %q %v", first, err)
	}
	// The recorder appends more data, then the user stops it.
	files, _ := filepath.Glob(filepath.Join(dir, "*.ts"))
	f, err := os.OpenFile(files[0], os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("BBBB")
	_ = f.Close()
	rest := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(live.Body)
		rest <- string(b)
	}()
	time.Sleep(700 * time.Millisecond) // > one poll tick: the tail must have been delivered live
	if r, err := http.Post(srv.URL+"/api/recordings/"+id+"/stop?api_password="+recPW, "", nil); err != nil || r.StatusCode != 200 {
		t.Fatalf("stop: %v %v", err, r)
	}
	select {
	case got := <-rest:
		if got != "BBBB" {
			t.Errorf("followed tail = %q; want BBBB", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("live stream did not end after the recording stopped")
	}
}

func dvrListIDs(t *testing.T, base string) []string {
	t.Helper()
	resp, err := http.Get(base + "/api/recordings?api_password=" + recPW)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	var ids []string
	for _, r := range decodeJSON(t, b)["recordings"].([]any) {
		ids = append(ids, r.(map[string]any)["id"].(string))
	}
	return ids
}

func TestServerCloseStopsRecordings(t *testing.T) {
	fake := &fakeRec{data: "KEEP"}
	h, dir := dvrHandler(t, fake, nil)
	startRec(t, h, "")
	if err := h.(io.Closer).Close(); err != nil {
		t.Fatal(err)
	}
	if fake.procs[0].n != 1 {
		t.Errorf("shutdown quit the recorder %d times", fake.procs[0].n)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.ts"))
	if len(files) != 1 {
		t.Fatalf("recording file dropped on shutdown: %v", files)
	}
	if b, _ := os.ReadFile(files[0]); string(b) != "KEEP" {
		t.Errorf("file = %q", b)
	}
}

func TestDVRRoutesAreSideEffecting(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{"GET", "/record?url=x", true},
		{"GET", "/record/stop/abc", true},
		{"GET", "/api/recordings/abc/delete", true},
		{"POST", "/api/recordings/start", true},
		{"POST", "/api/recordings/abc/stop", true},
		{"DELETE", "/api/recordings/abc", true},
		{"DELETE", "/api/recordings/all", true},
		{"GET", "/api/recordings", false},
		{"GET", "/api/recordings/abc/stream", false},
		{"GET", "/api/recordings/abc/download", false},
		{"GET", "/api/other", false},
	} {
		r := httptest.NewRequest(c.method, c.path, nil)
		if got := sideEffectingRoute(r); got != c.want {
			t.Errorf("%s %s side-effecting = %v; want %v", c.method, c.path, got, c.want)
		}
	}
}

func TestDVRCrossSiteGuard(t *testing.T) {
	fake := &fakeRec{data: "x"}
	h, _ := dvrHandler(t, fake, nil)
	id, _ := startRec(t, h, "")
	before := fake.count()
	pw := "api_password=" + recPW
	cross := map[string]string{"Sec-Fetch-Site": "cross-site"}
	// Browser-originated cross-site loads (<img>, <video>, fetch no-cors) must not
	// start, stop or delete anything — not even as a <video> load of /record.
	for _, c := range []struct{ method, path string }{
		{"GET", recQuery(recSrc+"?cs", "")},
		{"GET", "/record/stop/" + id + "?" + pw},
		{"GET", "/api/recordings/" + id + "/delete?" + pw},
		{"POST", "/api/recordings/start?" + pw},
		{"POST", "/api/recordings/" + id + "/stop?" + pw},
		{"DELETE", "/api/recordings/" + id + "?" + pw},
	} {
		for _, dest := range []string{"", "video", "image"} {
			hdr := map[string]string{"Sec-Fetch-Site": "cross-site"}
			if dest != "" {
				hdr["Sec-Fetch-Dest"] = dest
			}
			rec := do(h, c.method, c.path, hdr, nil)
			if rec.Code != http.StatusForbidden {
				t.Errorf("cross-site %s %s (dest=%q) = %d; want 403", c.method, c.path, dest, rec.Code)
			}
		}
	}
	if fake.count() != before {
		t.Error("cross-site request spawned a recorder")
	}
	if v, ok := decodeJSON(t, do(h, "GET", "/api/recordings/"+id+"?"+pw, nil, nil).Body.Bytes())["is_active"]; !ok || v != true {
		t.Error("cross-site stop/delete took effect")
	}
	// Playback and JSON reads stay open cross-site; header-less clients and
	// same-origin/same-site/none are not affected.
	// (the recording is still active, so the live-follow stream only ends when the client goes away)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	vreq := httptest.NewRequest("GET", "/api/recordings/"+id+"/stream?"+pw, nil).WithContext(ctx)
	vreq.Header.Set("Sec-Fetch-Site", "cross-site")
	vreq.Header.Set("Sec-Fetch-Dest", "video")
	vrec := httptest.NewRecorder()
	h.ServeHTTP(vrec, vreq)
	if vrec.Code == http.StatusForbidden {
		t.Error("cross-site <video> playback of a recording must stay allowed")
	}
	if rec := do(h, "GET", "/api/recordings?"+pw, cross, nil); rec.Code != 200 {
		t.Errorf("cross-site list = %d", rec.Code)
	}
	for _, site := range []string{"same-origin", "same-site", "none"} {
		if rec := do(h, "GET", recQuery(recSrc, ""), map[string]string{"Sec-Fetch-Site": site}, nil); rec.Code != http.StatusFound {
			t.Errorf("Sec-Fetch-Site=%s /record = %d", site, rec.Code)
		}
	}
	// A disallowed Origin is refused before routing.
	if rec := do(h, "POST", "/api/recordings/start?"+pw, map[string]string{"Origin": "https://evil.example"}, strings.NewReader(`{"url":"`+recSrc+`?o"}`)); rec.Code != http.StatusForbidden {
		t.Errorf("evil Origin POST start = %d; want 403", rec.Code)
	}
}

func TestDVRHostCheck(t *testing.T) {
	fake := &fakeRec{data: "x"}
	// With a password, DVR routes work under any Host (remote addons use the public name).
	h, _ := dvrHandler(t, fake, nil)
	req := httptest.NewRequest("GET", "/api/recordings?api_password="+recPW, nil)
	req.Host = "dvr.public.example"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("password-protected DVR under a public Host = %d", rec.Code)
	}
	// Without a password the DNS-rebinding Host guard still applies.
	h2, _ := dvrHandler(t, fake, func(c *types.Config) { c.ProxyPassword = "" })
	req = httptest.NewRequest("GET", "/api/recordings", nil)
	req.Host = "evil.rebind.example"
	rec = httptest.NewRecorder()
	h2.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("open DVR under a rebinding Host = %d; want 403", rec.Code)
	}
}

func TestDVRUnavailableWhenDirUnusable(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHandlerWithCfg(t, func(c *types.Config) {
		c.DVREnabled = true
		c.DVRDir = filepath.Join(blocker, "sub") // parent is a file
		c.ProxyPassword = recPW
	})
	rec := do(h, "GET", "/api/recordings?api_password="+recPW, nil, nil)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "DVR unavailable") {
		t.Errorf("= %d %s", rec.Code, rec.Body.String())
	}
}

func TestDVRDefaultDirIsUnderAppPath(t *testing.T) {
	fake := &fakeRec{data: "x"}
	app := t.TempDir()
	dvrStartFunc = fake.start
	dvrLookPath = func(string) (string, error) { return "ffmpeg", nil }
	t.Cleanup(func() { dvrStartFunc, dvrLookPath = nil, nil })
	h := newHandlerWithCfg(t, func(c *types.Config) {
		c.DVREnabled = true
		c.AppPath = app
		c.ProxyPassword = recPW
	})
	t.Cleanup(func() { _ = h.(io.Closer).Close() })
	startRec(t, h, "")
	if ents, err := os.ReadDir(filepath.Join(app, "recordings")); err != nil || len(ents) == 0 {
		t.Errorf("recordings not under APP_PATH/recordings: %v %v", ents, err)
	}
}
