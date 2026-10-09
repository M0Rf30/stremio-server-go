// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	lzstring "github.com/daku10/go-lz-string"
)

// Players such as ExoPlayer (Stremio Android) re-request the original
// GET /{ext}/create/{key}?lz=… stream URL on every open and seek. A create whose
// payload is equivalent to the live session's must therefore reuse that session
// (no new download, no new extraction); a different payload, a changed local
// file, a failed or an expired session must still build a fresh one.

// ── fixtures ─────────────────────────────────────────────────────────────────

// archReuseOrigin serves one archive over HTTP and counts the downloads.
type archReuseOrigin struct {
	srv  *httptest.Server
	gets atomic.Int32
	gate chan struct{} // when non-nil (set before the first request), responses wait on it
}

func newArchReuseOrigin(t *testing.T, data []byte) *archReuseOrigin {
	t.Helper()
	o := &archReuseOrigin{}
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		o.gets.Add(1)
		if o.gate != nil {
			<-o.gate
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(data)
	}))
	t.Cleanup(o.srv.Close)
	return o
}

// archReuseEnv lets create download from loopback and keeps the temp files it
// creates inside the test's own directory.
func archReuseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("STREMIO_ARCHIVE_ALLOW_PRIVATE", "1")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
}

// archReuseKey is a session key unique to the test; the session registered
// under it is torn down with the test.
func archReuseKey(t *testing.T) string {
	t.Helper()
	key := "reuse-" + strings.ReplaceAll(strings.ToLower(t.Name()), "/", "-")
	t.Cleanup(func() {
		archiveSessionsMu.Lock()
		cur := archiveSessions[key]
		delete(archiveSessions, key)
		archiveSessionsMu.Unlock()
		if cur != nil {
			archiveDestroySession(cur)
		}
	})
	return key
}

func archReuseSession(key string) *archiveSession {
	archiveSessionsMu.Lock()
	defer archiveSessionsMu.Unlock()
	return archiveSessions[key]
}

// archReuseCountExtractions counts the real extractions started in the test.
func archReuseCountExtractions(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	archiveExtractTestHook = func(string) { n.Add(1) }
	t.Cleanup(func() { archiveExtractTestHook = nil })
	return &n
}

var archReuseNoFollow = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// archReuseCreate issues the player-facing GET /{ext}/create/{key}?lz=… and
// returns the 307 Location. The client does not follow it.
func archReuseCreate(t testing.TB, base, ext, key, payload string) (loc string, status int) {
	t.Helper()
	lz, err := lzstring.CompressToEncodedURIComponent(payload)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := archReuseNoFollow.Get(base + "/" + ext + "/create/" + url.PathEscape(key) + "?lz=" + url.QueryEscape(lz))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.Header.Get("Location"), resp.StatusCode
}

// archReuseGet fetches path (a redirect Location) with an optional Range.
func archReuseGet(t testing.TB, base, path, rng string) (status int, body []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

// archReuseSeek is one ExoPlayer round trip: re-request the create URL, follow
// the 307, ask for a range of the entry.
func archReuseSeek(t testing.TB, base, ext, key, payload string, off int64, want []byte) (loc string) {
	t.Helper()
	loc, status := archReuseCreate(t, base, ext, key, payload)
	if status != http.StatusTemporaryRedirect {
		t.Fatalf("create: status %d, want 307", status)
	}
	end := off + 999
	st, body := archReuseGet(t, base, loc, fmt.Sprintf("bytes=%d-%d", off, end))
	if st != http.StatusPartialContent {
		t.Fatalf("range GET %s: status %d, want 206", loc, st)
	}
	if !bytes.Equal(body, want[off:end+1]) {
		t.Fatalf("range GET at %d returned wrong bytes", off)
	}
	return loc
}

// ── the measured scenario ────────────────────────────────────────────────────

// N identical create+range-GET round trips against a remote, deflated archive:
// the archive is downloaded once and the entry extracted once, all round trips
// share one session.
func TestArchiveCreate_RepeatedCreateReusesSession(t *testing.T) {
	archReuseEnv(t)
	content := progNoise(2<<20, 51)
	zipPath := progZip(t, progEntry{"movie.mkv", zip.Deflate, content})
	data, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	origin := newArchReuseOrigin(t, data)
	extractions := archReuseCountExtractions(t)
	srv := httptest.NewServer(newHandler(t))
	t.Cleanup(srv.Close)
	key := archReuseKey(t)
	payload := fmt.Sprintf(`{"url":%q}`, origin.srv.URL+"/a.zip")

	const n = 10
	var first *archiveSession
	for i := range n {
		loc := archReuseSeek(t, srv.URL, "zip", key, payload, int64(i)*150_000, content)
		if want := "/zip/stream/" + key + "/movie.mkv"; loc != want {
			t.Fatalf("round trip %d: Location = %q, want %q", i, loc, want)
		}
		cur := archReuseSession(key)
		if cur == nil {
			t.Fatalf("round trip %d: no session registered", i)
		}
		if first == nil {
			first = cur
		} else if cur != first {
			t.Errorf("round trip %d replaced the session", i)
		}
	}
	t.Logf("%d create+range round trips: %d downloads, %d extractions", n, origin.gets.Load(), extractions.Load())
	if got := origin.gets.Load(); got != 1 {
		t.Errorf("archive downloaded %d times, want 1", got)
	}
	if got := extractions.Load(); got != 1 {
		t.Errorf("entry extracted %d times, want 1", got)
	}
}

// The POST form of create answers with the key of the reused session.
func TestArchiveCreate_RepeatedPostReturnsSameKeyWithoutDownload(t *testing.T) {
	archReuseEnv(t)
	data, err := os.ReadFile(progZip(t, progEntry{"movie.mkv", zip.Deflate, progNoise(64<<10, 52)}))
	if err != nil {
		t.Fatal(err)
	}
	origin := newArchReuseOrigin(t, data)
	h := newHandler(t)
	key := archReuseKey(t)
	body := fmt.Sprintf(`{"url":%q}`, origin.srv.URL+"/a.zip")
	for i := range 3 {
		req := httptest.NewRequest(http.MethodPost, "/zip/create/"+key, strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST #%d: status %d: %s", i, rec.Code, rec.Body.String())
		}
		if got := strings.TrimSpace(rec.Body.String()); got != fmt.Sprintf(`{"key":%q}`, key) {
			t.Fatalf("POST #%d: body %s", i, got)
		}
	}
	if got := origin.gets.Load(); got != 1 {
		t.Errorf("archive downloaded %d times, want 1", got)
	}
}

// Spellings of the same request (array form, from/urls, null options, numeric
// fileIdx as a string, case of fileMustInclude) are the same payload.
func TestArchiveCreate_EquivalentPayloadsReuse(t *testing.T) {
	archReuseEnv(t)
	data, err := os.ReadFile(progZip(t, progEntry{"movie.mkv", zip.Deflate, progNoise(64<<10, 53)}))
	if err != nil {
		t.Fatal(err)
	}
	origin := newArchReuseOrigin(t, data)
	srv := httptest.NewServer(newHandler(t))
	t.Cleanup(srv.Close)
	u := origin.srv.URL + "/a.zip"
	cases := []struct {
		name  string
		first string
		again []string
	}{
		{"url spellings", fmt.Sprintf(`{"url":%q}`, u), []string{
			fmt.Sprintf(`[{"url":%q}]`, u),
			fmt.Sprintf(`{"from":%q}`, u),
			fmt.Sprintf(`{"urls":[[%q,123]]}`, u),
			fmt.Sprintf(`{"urls":[%q]}`, u),
			fmt.Sprintf(`{"urls":[{"url":%q}]}`, u),
			fmt.Sprintf(`{"url":%q,"fileIdx":null,"fileMustInclude":null}`, u),
			fmt.Sprintf(`{ "url" : %q , "unknown" : 1 }`, u),
		}},
		{"options", fmt.Sprintf(`{"url":%q,"fileIdx":0,"fileMustInclude":["MOVIE","x"]}`, u), []string{
			fmt.Sprintf(`{"url":%q,"fileIdx":"0","fileMustInclude":["movie","X"]}`, u),
		}},
		{"single filter", fmt.Sprintf(`{"url":%q,"fileMustInclude":"Movie"}`, u), []string{
			fmt.Sprintf(`{"url":%q,"fileMustInclude":["movie"]}`, u),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := archReuseKey(t)
			before := origin.gets.Load()
			archReuseCreate(t, srv.URL, "zip", key, tc.first)
			first := archReuseSession(key)
			if first == nil {
				t.Fatal("no session")
			}
			for _, p := range tc.again {
				if _, st := archReuseCreate(t, srv.URL, "zip", key, p); st != http.StatusTemporaryRedirect {
					t.Fatalf("create %s: status %d", p, st)
				}
				if archReuseSession(key) != first {
					t.Errorf("payload %s was not treated as equivalent", p)
				}
			}
			if got := origin.gets.Load() - before; got != 1 {
				t.Errorf("downloaded %d times, want 1", got)
			}
		})
	}
}

// ── what must NOT be reused ──────────────────────────────────────────────────

// A different payload under the same key replaces the session, and the reply
// reflects the new payload (not the old session's selection).
func TestArchiveCreate_DifferentPayloadReplacesSession(t *testing.T) {
	archReuseEnv(t)
	data, err := os.ReadFile(progZip(t,
		progEntry{"movie.mkv", zip.Deflate, progNoise(128<<10, 54)},
		progEntry{"extras/sample.mkv", zip.Deflate, progNoise(16<<10, 55)}))
	if err != nil {
		t.Fatal(err)
	}
	origin := newArchReuseOrigin(t, data)
	srv := httptest.NewServer(newHandler(t))
	t.Cleanup(srv.Close)
	key := archReuseKey(t)
	u := origin.srv.URL + "/a.zip"

	steps := []struct {
		payload string
		wantLoc string
		wantNew bool
		wantDLs int32
	}{
		{fmt.Sprintf(`{"url":%q}`, u), "/zip/stream/" + key + "/movie.mkv", true, 1},
		{fmt.Sprintf(`{"url":%q}`, u), "/zip/stream/" + key + "/movie.mkv", false, 1},
		{fmt.Sprintf(`{"url":%q,"fileIdx":1}`, u), "/zip/stream/" + key + "/extras/sample.mkv", true, 2},
		{fmt.Sprintf(`{"url":%q,"fileIdx":1}`, u), "/zip/stream/" + key + "/extras/sample.mkv", false, 2},
		{fmt.Sprintf(`{"url":%q,"fileMustInclude":"sample"}`, u), "/zip/stream/" + key + "/extras/sample.mkv", true, 3},
		{fmt.Sprintf(`{"url":%q,"fileMustInclude":"movie"}`, u), "/zip/stream/" + key + "/movie.mkv", true, 4},
		{fmt.Sprintf(`{"url":%q}`, u+"?v=2"), "/zip/stream/" + key + "/movie.mkv", true, 5},
	}
	var prev *archiveSession
	for i, st := range steps {
		loc, code := archReuseCreate(t, srv.URL, "zip", key, st.payload)
		if code != http.StatusTemporaryRedirect {
			t.Fatalf("step %d: status %d", i, code)
		}
		if loc != st.wantLoc {
			t.Errorf("step %d: Location = %q, want %q", i, loc, st.wantLoc)
		}
		cur := archReuseSession(key)
		if st.wantNew == (cur == prev) {
			t.Errorf("step %d: new session = %v, want %v", i, cur != prev, st.wantNew)
		}
		if got := origin.gets.Load(); got != st.wantDLs {
			t.Errorf("step %d: %d downloads, want %d", i, got, st.wantDLs)
		}
		prev = cur
	}
}

// With a local archive the file's size and mtime guard the reuse.
func TestArchiveCreate_LocalFileChangeRebuilds(t *testing.T) {
	old := progNoise(256<<10, 56)
	changed := progNoise(300<<10, 57)
	dir := t.TempDir()
	t.Setenv("STREMIO_ARCHIVE_LOCAL_ROOT", dir)
	p := filepath.Join(dir, "m.zip")
	write := func(content []byte) {
		t.Helper()
		src := progZip(t, progEntry{"movie.mkv", zip.Deflate, content})
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(old)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	setMTime := func(d time.Duration) {
		t.Helper()
		if err := os.Chtimes(p, base.Add(d), base.Add(d)); err != nil {
			t.Fatal(err)
		}
	}
	setMTime(0)

	extractions := archReuseCountExtractions(t)
	srv := httptest.NewServer(newHandler(t))
	t.Cleanup(srv.Close)
	key := archReuseKey(t)
	payload := fmt.Sprintf(`{"url":%q}`, p)

	seek := func(want []byte) *archiveSession {
		t.Helper()
		archReuseSeek(t, srv.URL, "zip", key, payload, 1000, want)
		return archReuseSession(key)
	}

	s1 := seek(old)
	if s2 := seek(old); s2 != s1 {
		t.Fatal("unchanged local archive was not reused")
	}
	if got := extractions.Load(); got != 1 {
		t.Fatalf("%d extractions for an unchanged archive, want 1", got)
	}

	// Same size, newer mtime (e.g. restored/touched file).
	setMTime(2 * time.Second)
	s3 := seek(old)
	if s3 == s1 {
		t.Error("changed mtime did not rebuild the session")
	}
	if s4 := seek(old); s4 != s3 {
		t.Error("session built after an mtime change is not reused again")
	}

	// Different content and size, mtime left as it was.
	write(changed)
	setMTime(2 * time.Second)
	s5 := seek(changed)
	if s5 == s3 {
		t.Error("changed size did not rebuild the session")
	}

	// The file disappears: create fails as it always did.
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, st := archReuseCreate(t, srv.URL, "zip", key, payload); st != http.StatusBadRequest {
		t.Errorf("create for a vanished local archive: status %d, want 400", st)
	}
}

// A session whose extraction failed (corrupt archive) is not reused: the retry
// builds a fresh one instead of replaying the failure.
func TestArchiveCreate_FailedSessionRebuilds(t *testing.T) {
	t.Run("corrupt deflate entry", func(t *testing.T) {
		content := progNoise(512<<10, 58)
		p := progZip(t, progEntry{"movie.mkv", zip.Deflate, content})
		zr, err := zip.OpenReader(p)
		if err != nil {
			t.Fatal(err)
		}
		off, err := zr.File[0].DataOffset()
		_ = zr.Close()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		raw[off+int64(len(content))/2] ^= 0xff // CRC mismatch at the end of the extraction
		if err := os.WriteFile(p, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		archReuseFailedRetry(t, p, len(content))
	})
	t.Run("corrupt stored entry", func(t *testing.T) {
		content := progNoise(256<<10, 59)
		p := progZip(t, progEntry{"movie.mkv", zip.Store, content})
		corruptStoredPayload(t, p, 0, int64(len(content))/2)
		archReuseFailedRetry(t, p, len(content))
	})
}

func archReuseFailedRetry(t *testing.T, p string, size int) {
	t.Helper()
	t.Setenv("STREMIO_ARCHIVE_LOCAL_ROOT", filepath.Dir(p))
	srv := httptest.NewServer(newHandler(t))
	t.Cleanup(srv.Close)
	key := archReuseKey(t)
	payload := fmt.Sprintf(`{"url":%q}`, p)

	loc, st := archReuseCreate(t, srv.URL, "zip", key, payload)
	if st != http.StatusTemporaryRedirect {
		t.Fatalf("create: status %d", st)
	}
	failed := archReuseSession(key)
	resp, err := http.Get(srv.URL + loc)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body) // a truncated response is the expected outcome
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK && len(body) >= size {
		t.Fatalf("corrupt entry was delivered complete (%d bytes)", len(body))
	}
	progWaitFlights(t, failed)

	if _, st := archReuseCreate(t, srv.URL, "zip", key, payload); st != http.StatusTemporaryRedirect {
		t.Fatalf("retry create: status %d", st)
	}
	if archReuseSession(key) == failed {
		t.Error("the failed session was reused instead of rebuilt")
	}
}

// Reuse is bounded by the session TTL (counted from the build, so a remote
// archive that changed upstream is picked up at most one TTL later).
func TestArchiveCreate_ExpiredSessionRebuilds(t *testing.T) {
	archReuseEnv(t)
	data, err := os.ReadFile(progZip(t, progEntry{"movie.mkv", zip.Deflate, progNoise(64<<10, 60)}))
	if err != nil {
		t.Fatal(err)
	}
	origin := newArchReuseOrigin(t, data)
	srv := httptest.NewServer(newHandler(t))
	t.Cleanup(srv.Close)
	key := archReuseKey(t)
	payload := fmt.Sprintf(`{"url":%q}`, origin.srv.URL+"/a.zip")

	archReuseCreate(t, srv.URL, "zip", key, payload)
	s1 := archReuseSession(key)
	archReuseCreate(t, srv.URL, "zip", key, payload)
	if archReuseSession(key) != s1 {
		t.Fatal("fresh session was not reused")
	}

	s1.mu.Lock()
	s1.created = time.Now().Add(-archiveSessionTTL - time.Minute)
	s1.mu.Unlock()
	archReuseCreate(t, srv.URL, "zip", key, payload)
	if archReuseSession(key) == s1 {
		t.Error("session older than the TTL was reused")
	}
	if got := origin.gets.Load(); got != 2 {
		t.Errorf("%d downloads, want 2", got)
	}
}

// A download that failed leaves nothing behind to reuse: the retry downloads again.
func TestArchiveCreate_FailedDownloadIsRetried(t *testing.T) {
	archReuseEnv(t)
	data, err := os.ReadFile(progZip(t, progEntry{"movie.mkv", zip.Deflate, progNoise(32<<10, 61)}))
	if err != nil {
		t.Fatal(err)
	}
	var gets atomic.Int32
	var failing atomic.Bool
	failing.Store(true)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		gets.Add(1)
		if failing.Load() {
			http.Error(w, "nope", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(origin.Close)
	srv := httptest.NewServer(newHandler(t))
	t.Cleanup(srv.Close)
	key := archReuseKey(t)
	payload := fmt.Sprintf(`{"url":%q}`, origin.URL+"/a.zip")

	if _, st := archReuseCreate(t, srv.URL, "zip", key, payload); st != http.StatusBadGateway {
		t.Fatalf("create against a failing origin: status %d, want 502", st)
	}
	failing.Store(false)
	if _, st := archReuseCreate(t, srv.URL, "zip", key, payload); st != http.StatusTemporaryRedirect {
		t.Fatalf("retry create: status %d, want 307", st)
	}
	if got := gets.Load(); got != 2 {
		t.Errorf("%d downloads, want 2 (the failure must not be remembered)", got)
	}
}

// ── concurrency ──────────────────────────────────────────────────────────────

// Parallel identical creates (a player opening several connections at once)
// build one session: one download, and everybody is sent to the same stream URL.
func TestArchiveCreate_ConcurrentIdenticalCreatesBuildOnce(t *testing.T) {
	archReuseEnv(t)
	content := progNoise(256<<10, 62)
	data, err := os.ReadFile(progZip(t, progEntry{"movie.mkv", zip.Deflate, content}))
	if err != nil {
		t.Fatal(err)
	}
	origin := newArchReuseOrigin(t, data)
	origin.gate = make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(origin.gate) }) }
	t.Cleanup(release)
	srv := httptest.NewServer(newHandler(t))
	t.Cleanup(srv.Close)
	key := archReuseKey(t)
	payload := fmt.Sprintf(`{"url":%q}`, origin.srv.URL+"/a.zip")

	const workers = 8
	type result struct {
		loc    string
		status int
	}
	results := make(chan result, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			loc, st := archReuseCreate(t, srv.URL, "zip", key, payload)
			results <- result{loc, st}
		}()
	}
	// Let the leader reach the origin and the others queue up behind it.
	deadline := time.Now().Add(10 * time.Second)
	for origin.gets.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	release()
	wg.Wait()
	close(results)

	want := "/zip/stream/" + key + "/movie.mkv"
	for r := range results {
		if r.status != http.StatusTemporaryRedirect || r.loc != want {
			t.Errorf("create: status %d, Location %q; want 307 %q", r.status, r.loc, want)
		}
	}
	if got := origin.gets.Load(); got != 1 {
		t.Errorf("archive downloaded %d times for %d parallel creates, want 1", got, workers)
	}
	if st, body := archReuseGet(t, srv.URL, want, "bytes=0-99"); st != http.StatusPartialContent || !bytes.Equal(body, content[:100]) {
		t.Errorf("the shared session does not serve: status %d", st)
	}
}

// ── single-flight and bookkeeping ────────────────────────────────────────────

// A create that arrives while an identical build is running joins it and
// answers with the leader's outcome — here its failure; a create for another
// payload builds on its own. Once the leader is done nothing is left behind.
func TestArchiveReuseOrLead_JoinsInFlightBuildAndSharesFailure(t *testing.T) {
	const key, sig = "flight-key", `{"sig":"a"}`
	t.Cleanup(func() {
		archiveSessionsMu.Lock()
		delete(archiveBuilds, archiveBuildKey{key, sig})
		delete(archiveBuilds, archiveBuildKey{key, "other"})
		archiveSessionsMu.Unlock()
	})

	_, lead, leader := archiveReuseOrLead(key, sig)
	if !leader || lead == nil {
		t.Fatal("first create must lead the build")
	}
	if sess, fl, leader := archiveReuseOrLead(key, sig); sess != nil || leader || fl != lead {
		t.Fatalf("identical create must join the build in progress (sess=%v leader=%v same flight=%v)", sess, leader, fl == lead)
	}
	if _, fl, leader := archiveReuseOrLead(key, "other"); !leader || fl == lead {
		t.Fatal("a different payload must not wait for the other build")
	}

	// The leader fails (local archives are disabled here): waiters get the same answer.
	t.Setenv("STREMIO_ARCHIVE_LOCAL_ROOT", "")
	t.Setenv("LOCAL_FILES_DIR", "")
	payload := &archivePayload{URL: "/nowhere/a.zip"}
	res := archiveBuildAndRegister("zip", key, sig, lead, payload, payload.URL)
	if res.status != http.StatusBadRequest || res.msg != archiveLocalPathErrMsg {
		t.Fatalf("leader result = %+v", res)
	}
	select {
	case <-lead.done:
	default:
		t.Fatal("flight not completed")
	}
	if lead.res != res {
		t.Errorf("waiters would see %+v, leader answered %+v", lead.res, res)
	}
	archiveSessionsMu.Lock()
	_, stale := archiveBuilds[archiveBuildKey{key, sig}]
	archiveSessionsMu.Unlock()
	if stale {
		t.Error("completed flight is still registered")
	}
	// The failure is not remembered: the next create leads a new build.
	if _, fl, leader := archiveReuseOrLead(key, sig); !leader || fl == lead {
		t.Error("a create after a failed build must build again")
	}
}

// Even if building panics the flight is completed, so waiters are never stranded.
func TestArchiveBuildAndRegister_PanicStillCompletesFlight(t *testing.T) {
	const key, sig = "panic-key", `{"sig":"p"}`
	t.Cleanup(func() {
		archiveSessionsMu.Lock()
		delete(archiveBuilds, archiveBuildKey{key, sig})
		archiveSessionsMu.Unlock()
	})
	_, fl, leader := archiveReuseOrLead(key, sig)
	if !leader {
		t.Fatal("expected to lead")
	}
	func() {
		defer func() { _ = recover() }()
		archiveBuildAndRegister("zip", key, sig, fl, nil, "x") // nil payload panics
	}()
	select {
	case <-fl.done:
	default:
		t.Fatal("flight left open after a panic")
	}
	if fl.res.status != http.StatusInternalServerError {
		t.Errorf("waiters would see %+v", fl.res)
	}
	archiveSessionsMu.Lock()
	_, stale := archiveBuilds[archiveBuildKey{key, sig}]
	archiveSessionsMu.Unlock()
	if stale {
		t.Error("flight still registered after a panic")
	}
}

// Reusing a session refreshes its idle clock (so the janitor keeps it while the
// player keeps seeking) and leaves nothing pinned.
func TestArchiveCreate_ReuseRefreshesLastAccessAndUnpins(t *testing.T) {
	archReuseEnv(t)
	data, err := os.ReadFile(progZip(t, progEntry{"movie.mkv", zip.Deflate, progNoise(32<<10, 63)}))
	if err != nil {
		t.Fatal(err)
	}
	origin := newArchReuseOrigin(t, data)
	srv := httptest.NewServer(newHandler(t))
	t.Cleanup(srv.Close)
	key := archReuseKey(t)
	payload := fmt.Sprintf(`{"url":%q}`, origin.srv.URL+"/a.zip")

	archReuseCreate(t, srv.URL, "zip", key, payload)
	sess := archReuseSession(key)
	stale := time.Now().Add(-archiveSessionTTL / 2)
	sess.mu.Lock()
	sess.lastAccess = stale
	sess.mu.Unlock()

	archReuseCreate(t, srv.URL, "zip", key, payload)
	if archReuseSession(key) != sess {
		t.Fatal("session was not reused")
	}
	sess.mu.Lock()
	last, refs, noReuse := sess.lastAccess, sess.refCount, sess.noReuse
	sess.mu.Unlock()
	if !last.After(stale) {
		t.Error("reuse did not refresh lastAccess")
	}
	if refs != 0 {
		t.Errorf("refCount = %d after the create returned, want 0", refs)
	}
	if noReuse {
		t.Error("a healthy session was marked failed")
	}
	archiveSessionsMu.Lock()
	builds := len(archiveBuilds)
	archiveSessionsMu.Unlock()
	if builds != 0 {
		t.Errorf("%d build flights left registered", builds)
	}
}

// A checksum mismatch found by the background verification of a large stored
// entry (no request has reached the entry's end yet) already makes the session
// non-reusable: the next create rebuilds.
func TestArchiveCreate_BackgroundChecksumFailureMarksSessionFailed(t *testing.T) {
	setEagerVerify(t, 4<<10)
	content := progNoise(1<<20, 64)
	p := progZip(t, progEntry{"movie.mkv", zip.Store, content})
	corruptStoredPayload(t, p, 0, 700_000)
	h := newHandler(t)
	key, sess := progCreate(t, h, p, "zip")

	// A head range starts the verification (and is answered unverified).
	if rec := progGet(h, http.MethodGet, "zip", key, "movie.mkv", "bytes=0-99"); rec.Code != http.StatusPartialContent {
		t.Fatalf("head range: status %d", rec.Code)
	}
	auditEventually(t, 15*time.Second, "background verification to mark the session failed", func() bool {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return sess.noReuse
	})

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	if _, st := archReuseCreate(t, srv.URL, "zip", key, fmt.Sprintf(`{"url":%q}`, p)); st != http.StatusTemporaryRedirect {
		t.Fatalf("create: status %d", st)
	}
	fresh := archReuseSession(key)
	if fresh == sess {
		t.Error("the session with a known checksum mismatch was reused")
	}
	t.Cleanup(func() {
		archiveSessionsMu.Lock()
		if archiveSessions[key] == fresh {
			delete(archiveSessions, key)
		}
		archiveSessionsMu.Unlock()
		archiveDestroySession(fresh)
	})
}
