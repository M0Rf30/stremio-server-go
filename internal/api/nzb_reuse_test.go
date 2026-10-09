// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

// Package api — tests for NZB session reuse: ExoPlayer re-requests
// GET /nzb/create/{key}?lz=… on every open/seek, and an equivalent payload must
// keep the live session (and the file it is assembling) instead of fetching the
// NZB again and re-downloading every segment over NNTP.
package api

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	lzstring "github.com/daku10/go-lz-string"

	"github.com/M0Rf30/stremio-server-go/internal/nzb"
)

// nzbReuseFixture is an NZB served over HTTP (counting the fetches) whose
// articles live on a counting fake NNTP server, behind the real create handler.
type nzbReuseFixture struct {
	*nzbStreamFixture
	nzbGets atomic.Int32
	nzbGate chan struct{} // when non-nil (set before the first request), NZB fetches wait on it
	nzbURL  string
	nntp    string
	key     string
}

func newNzbReuseFixture(t *testing.T, nseg, segSize, lastShort, connections int) *nzbReuseFixture {
	t.Helper()
	nzbReplaceEnv(t)
	rf := &nzbReuseFixture{nzbStreamFixture: newNzbStreamFixture(t, "movie.mkv", nseg, segSize, lastShort, connections)}
	var xml strings.Builder
	xml.WriteString(`<?xml version="1.0" encoding="UTF-8"?><nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">`)
	fmt.Fprintf(&xml, `<file subject="&quot;movie.mkv&quot; yEnc" poster="t@t"><segments>`)
	for _, s := range rf.file.Segments {
		fmt.Fprintf(&xml, `<segment bytes="%d" number="%d">%s</segment>`, s.Bytes, s.Number, s.MessageID)
	}
	xml.WriteString(`</segments></file></nzb>`)
	nzbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rf.nzbGets.Add(1)
		if rf.nzbGate != nil {
			<-rf.nzbGate
		}
		w.Header().Set("Content-Type", "application/x-nzb")
		_, _ = io.WriteString(w, xml.String())
	}))
	t.Cleanup(nzbSrv.Close)
	rf.nzbURL = nzbSrv.URL + "/movie.nzb"
	rf.nntp = "nntp://" + net.JoinHostPort("127.0.0.1", fmt.Sprint(rf.fake.ln.Addr().(*net.TCPAddr).Port))
	rf.key = "nzbreuse-" + strings.ReplaceAll(strings.ToLower(t.Name()), "/", "-")
	t.Cleanup(func() {
		nzbSessionsMu.Lock()
		cur := nzbSessions[rf.key]
		delete(nzbSessions, rf.key)
		nzbSessionsMu.Unlock()
		if cur != nil {
			cur.discard()
		}
	})
	return rf
}

// payload is the create payload; opts are extra top-level JSON members.
func (rf *nzbReuseFixture) payload(opts string) string {
	if opts != "" {
		opts = "," + opts
	}
	return fmt.Sprintf(`{"nzbUrl":%q,"servers":[%q]%s}`, rf.nzbURL, rf.nntp, opts)
}

func (rf *nzbReuseFixture) session() *nzbSession {
	nzbSessionsMu.Lock()
	defer nzbSessionsMu.Unlock()
	return nzbSessions[rf.key]
}

// create issues the player-facing GET /nzb/create/{key}?lz=… (no redirect follow).
func (rf *nzbReuseFixture) create(t testing.TB, payload string) (loc string, status int) {
	t.Helper()
	lz, err := lzstring.CompressToEncodedURIComponent(payload)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := archReuseNoFollow.Get(rf.srv.URL + "/nzb/create/" + url.PathEscape(rf.key) + "?lz=" + url.QueryEscape(lz))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.Header.Get("Location"), resp.StatusCode
}

// seek is one ExoPlayer round trip: re-request the create URL, follow the 307,
// ask for a range of the file.
func (rf *nzbReuseFixture) seek(t testing.TB, payload string, off int64) {
	t.Helper()
	loc, st := rf.create(t, payload)
	if st != http.StatusTemporaryRedirect {
		t.Fatalf("create: status %d, want 307", st)
	}
	end := off + 999
	status, body := archReuseGet(t, rf.srv.URL, loc, fmt.Sprintf("bytes=%d-%d", off, end))
	if status != http.StatusPartialContent {
		t.Fatalf("range GET %s: status %d, want 206", loc, status)
	}
	if !bytes.Equal(body, rf.content[off:end+1]) {
		t.Fatalf("range GET at %d returned wrong bytes", off)
	}
}

// totalBodies is the number of BODY commands the fake NNTP server has served.
func (rf *nzbReuseFixture) totalBodies() int {
	n := 0
	for _, s := range rf.file.Segments {
		n += rf.fake.bodies(s.MessageID)
	}
	return n
}

// waitComplete waits until the key's session has assembled the whole file.
func (rf *nzbReuseFixture) waitComplete(t *testing.T) {
	t.Helper()
	sess := rf.session()
	if sess == nil {
		t.Fatal("no session")
	}
	sess.mu.Lock()
	fs := sess.fileStates[rf.file.Name]
	sess.mu.Unlock()
	if fs == nil {
		t.Fatal("no assembly started")
	}
	fs.mu.Lock()
	asm := fs.asm
	fs.mu.Unlock()
	select {
	case <-asm.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("assembly did not finish")
	}
}

// ── the measured scenario ────────────────────────────────────────────────────

// N identical create+range-GET round trips: the NZB is fetched once and every
// article is downloaded once, all round trips share one session.
func TestNzbCreate_RepeatedCreateReusesSession(t *testing.T) {
	rf := newNzbReuseFixture(t, 12, 8_000, 500, 3)
	payload := rf.payload("")

	const n = 10
	var first *nzbSession
	for i := range n {
		rf.seek(t, payload, int64(i)*9_000)
		cur := rf.session()
		if cur == nil {
			t.Fatalf("round trip %d: no session", i)
		}
		if first == nil {
			first = cur
		} else if cur != first {
			t.Errorf("round trip %d replaced the session", i)
		}
	}
	rf.waitComplete(t)
	t.Logf("%d create+range round trips: %d NZB fetches, %d NNTP article downloads for %d segments",
		n, rf.nzbGets.Load(), rf.totalBodies(), len(rf.file.Segments))
	if got := rf.nzbGets.Load(); got != 1 {
		t.Errorf("NZB fetched %d times, want 1", got)
	}
	if got, want := rf.totalBodies(), len(rf.file.Segments); got != want {
		t.Errorf("%d articles downloaded for a %d-segment file, want each once", got, want)
	}
}

// The POST form answers with the reused session's key.
func TestNzbCreate_RepeatedPostReturnsSameKeyWithoutFetch(t *testing.T) {
	rf := newNzbReuseFixture(t, 4, 4_000, 0, 2)
	h := newHandler(t)
	body := rf.payload("")
	for i := range 3 {
		req := httptest.NewRequest(http.MethodPost, "/nzb/create/"+rf.key, strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST #%d: status %d: %s", i, rec.Code, rec.Body.String())
		}
		if got := strings.TrimSpace(rec.Body.String()); got != fmt.Sprintf(`{"key":%q}`, rf.key) {
			t.Fatalf("POST #%d: body %s", i, got)
		}
	}
	if got := rf.nzbGets.Load(); got != 1 {
		t.Errorf("NZB fetched %d times, want 1", got)
	}
}

// Spellings of the same request are the same payload: nzbUrls[0] for nzbUrl,
// legacy server objects for URLs, unspecified port/connections for their defaults.
func TestNzbCreate_EquivalentPayloadsReuse(t *testing.T) {
	rf := newNzbReuseFixture(t, 4, 4_000, 0, 2)
	port := rf.fake.ln.Addr().(*net.TCPAddr).Port
	rf.create(t, rf.payload(""))
	first := rf.session()
	if first == nil {
		t.Fatal("no session")
	}
	for _, p := range []string{
		fmt.Sprintf(`{"nzbUrls":[%q],"servers":[%q]}`, rf.nzbURL, rf.nntp),
		fmt.Sprintf(`{"nzbUrl":%q,"nzbUrls":["http://ignored.invalid/x.nzb"],"servers":[%q]}`, rf.nzbURL, rf.nntp),
		fmt.Sprintf(`{"nzbUrl":%q,"servers":[{"host":"127.0.0.1","port":%d}]}`, rf.nzbURL, port),
		fmt.Sprintf(`{"nzbUrl":%q,"servers":[%q,"nntp://other.example.com"]}`, rf.nzbURL, rf.nntp), // only the first server is used
	} {
		if _, st := rf.create(t, p); st != http.StatusTemporaryRedirect {
			t.Fatalf("create %s: status %d", p, st)
		}
		if rf.session() != first {
			t.Errorf("payload %s was not treated as equivalent", p)
		}
	}
	if got := rf.nzbGets.Load(); got != 1 {
		t.Errorf("NZB fetched %d times, want 1", got)
	}
}

// ── what must NOT be reused ──────────────────────────────────────────────────

// A different NZB URL, server, credentials or connection count under the same
// key replaces the session; the superseded payload is not remembered.
func TestNzbCreate_DifferentPayloadReplacesSession(t *testing.T) {
	rf := newNzbReuseFixture(t, 4, 4_000, 0, 2)
	port := rf.fake.ln.Addr().(*net.TCPAddr).Port
	steps := []struct {
		payload string
		wantNew bool
	}{
		{rf.payload(""), true},
		{rf.payload(""), false},
		{fmt.Sprintf(`{"nzbUrl":%q,"servers":[%q]}`, rf.nzbURL+"?v=2", rf.nntp), true},
		{fmt.Sprintf(`{"nzbUrl":%q,"servers":[%q]}`, rf.nzbURL+"?v=2", rf.nntp), false},
		{fmt.Sprintf(`{"nzbUrl":%q,"servers":["nntp://user:pw@127.0.0.1:%d"]}`, rf.nzbURL+"?v=2", port), true},
		{fmt.Sprintf(`{"nzbUrl":%q,"servers":["nntps://user:pw@127.0.0.1:%d"]}`, rf.nzbURL+"?v=2", port), true}, // TLS
		{fmt.Sprintf(`{"nzbUrl":%q,"servers":[{"host":"127.0.0.1","port":%d,"connections":7}]}`, rf.nzbURL+"?v=2", port), true},
		{rf.payload(""), true},
	}
	var prev *nzbSession
	for i, st := range steps {
		if _, code := rf.create(t, st.payload); code != http.StatusTemporaryRedirect {
			t.Fatalf("step %d: status %d", i, code)
		}
		cur := rf.session()
		if cur == nil {
			t.Fatalf("step %d: no session", i)
		}
		if isNew := cur != prev; isNew != st.wantNew {
			t.Errorf("step %d: new session = %v, want %v", i, isNew, st.wantNew)
		}
		prev = cur
	}
	if got, want := rf.nzbGets.Load(), int32(6); got != want {
		t.Errorf("NZB fetched %d times, want %d (one per distinct payload)", got, want)
	}
}

// A session whose assembly failed is not reused: the retry builds a fresh one
// (and so re-fetches the NZB and starts a clean assembly).
func TestNzbCreate_FailedSessionRebuilds(t *testing.T) {
	rf := newNzbReuseFixture(t, 6, 4_000, 0, 2)
	payload := rf.payload("")
	saved := rf.articles[rf.segID(0)]
	rf.fake.setArticle(rf.segID(0), nil) // 430: the first article is gone

	loc, st := rf.create(t, payload)
	if st != http.StatusTemporaryRedirect {
		t.Fatalf("create: status %d", st)
	}
	failed := rf.session()
	if status, _ := archReuseGet(t, rf.srv.URL, loc, ""); status != http.StatusBadGateway {
		t.Fatalf("stream of a broken NZB: status %d, want 502", status)
	}

	rf.fake.setArticle(rf.segID(0), saved) // the article is back
	if _, st := rf.create(t, payload); st != http.StatusTemporaryRedirect {
		t.Fatalf("retry create: status %d", st)
	}
	if rf.session() == failed {
		t.Fatal("the failed session was reused instead of rebuilt")
	}
	if got := rf.nzbGets.Load(); got != 2 {
		t.Errorf("NZB fetched %d times, want 2", got)
	}
	rf.seek(t, payload, 2_000)
	rf.waitComplete(t)
}

// Reuse is bounded by the session TTL, counted from the build.
func TestNzbCreate_ExpiredSessionRebuilds(t *testing.T) {
	rf := newNzbReuseFixture(t, 4, 4_000, 0, 2)
	payload := rf.payload("")
	rf.create(t, payload)
	s1 := rf.session()
	rf.create(t, payload)
	if rf.session() != s1 {
		t.Fatal("fresh session was not reused")
	}
	nzbSessionsMu.Lock()
	s1.created = time.Now().Add(-nzbSessionTTL - time.Minute)
	nzbSessionsMu.Unlock()
	rf.create(t, payload)
	if rf.session() == s1 {
		t.Error("session older than the TTL was reused")
	}
	if got := rf.nzbGets.Load(); got != 2 {
		t.Errorf("NZB fetched %d times, want 2", got)
	}
}

// A failed NZB fetch leaves nothing behind to reuse: the retry fetches again.
func TestNzbCreate_FailedFetchIsRetried(t *testing.T) {
	rf := newNzbReuseFixture(t, 4, 4_000, 0, 2)
	var gets atomic.Int32
	var failing atomic.Bool
	failing.Store(true)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		if failing.Load() {
			http.Error(w, "nope", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, rf.nzbURL, http.StatusFound)
	}))
	t.Cleanup(bad.Close)
	payload := fmt.Sprintf(`{"nzbUrl":%q,"servers":[%q]}`, bad.URL+"/x.nzb", rf.nntp)

	if _, st := rf.create(t, payload); st != http.StatusBadGateway {
		t.Fatalf("create against a failing origin: status %d, want 502", st)
	}
	failing.Store(false)
	if _, st := rf.create(t, payload); st != http.StatusTemporaryRedirect {
		t.Fatalf("retry create: status %d, want 307", st)
	}
	if got := gets.Load(); got != 2 {
		t.Errorf("%d fetches, want 2 (the failure must not be remembered)", got)
	}
}

// ── concurrency ──────────────────────────────────────────────────────────────

// Parallel identical creates build one session: one NZB fetch, one temp dir,
// everybody is sent to the same stream URL.
func TestNzbCreate_ConcurrentIdenticalCreatesBuildOnce(t *testing.T) {
	rf := newNzbReuseFixture(t, 6, 4_000, 0, 2)
	rf.nzbGate = make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(rf.nzbGate) }) }
	t.Cleanup(release)
	payload := rf.payload("")

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
			loc, st := rf.create(t, payload)
			results <- result{loc, st}
		}()
	}
	// Let the leader reach the NZB origin and the others queue up behind it.
	deadline := time.Now().Add(10 * time.Second)
	for rf.nzbGets.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	release()
	wg.Wait()
	close(results)

	want := "/nzb/stream/" + rf.key
	for r := range results {
		if r.status != http.StatusTemporaryRedirect || r.loc != want {
			t.Errorf("create: status %d, Location %q; want 307 %q", r.status, r.loc, want)
		}
	}
	if got := rf.nzbGets.Load(); got != 1 {
		t.Errorf("NZB fetched %d times for %d parallel creates, want 1", got, workers)
	}
	// Exactly one session dir (plus the fixture's own) exists in the isolated TMPDIR.
	var dirs int
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), nzbTmpDirPrefix) {
			dirs++
		}
	}
	if dirs != 1 {
		t.Errorf("%d session temp dirs after %d parallel creates, want 1", dirs, workers)
	}
	rf.seek(t, payload, 1_000)
}

// ── single-flight and bookkeeping ────────────────────────────────────────────

// A create that arrives while an identical build is running joins it and
// answers with the leader's outcome — here its failure; a create for another
// payload builds on its own. Once the leader is done nothing is left behind.
func TestNzbReuseOrLead_JoinsInFlightBuildAndSharesFailure(t *testing.T) {
	t.Setenv("STREMIO_ARCHIVE_ALLOW_PRIVATE", "1")
	const key, sig = "nzb-flight-key", `["sig"]`
	t.Cleanup(func() {
		nzbSessionsMu.Lock()
		delete(nzbBuilds, nzbBuildKey{key, sig})
		delete(nzbBuilds, nzbBuildKey{key, "other"})
		nzbSessionsMu.Unlock()
	})

	_, lead, leader := nzbReuseOrLead(key, sig)
	if !leader || lead == nil {
		t.Fatal("first create must lead the build")
	}
	if sess, fl, leader := nzbReuseOrLead(key, sig); sess != nil || leader || fl != lead {
		t.Fatalf("identical create must join the build in progress (sess=%v leader=%v same flight=%v)", sess, leader, fl == lead)
	}
	if _, fl, leader := nzbReuseOrLead(key, "other"); !leader || fl == lead {
		t.Fatal("a different payload must not wait for the other build")
	}

	// The leader fails (nothing listens on the NZB URL): waiters get the same answer.
	res := nzbBuildAndRegister(key, sig, lead, "http://127.0.0.1:1/x.nzb", nzb.ServerConfig{Host: "127.0.0.1", Port: 119})
	if res.status != http.StatusBadGateway {
		t.Fatalf("leader result = %+v, want 502", res)
	}
	select {
	case <-lead.done:
	default:
		t.Fatal("flight not completed")
	}
	if lead.res != res {
		t.Errorf("waiters would see %+v, leader answered %+v", lead.res, res)
	}
	nzbSessionsMu.Lock()
	_, stale := nzbBuilds[nzbBuildKey{key, sig}]
	nzbSessionsMu.Unlock()
	if stale {
		t.Error("completed flight is still registered")
	}
	// The failure is not remembered: the next create leads a new build.
	if _, fl, leader := nzbReuseOrLead(key, sig); !leader || fl == lead {
		t.Error("a create after a failed build must build again")
	}
}

// Reusing a session refreshes its idle clock and leaves nothing pinned.
func TestNzbCreate_ReuseRefreshesLastAccessAndUnpins(t *testing.T) {
	rf := newNzbReuseFixture(t, 4, 4_000, 0, 2)
	payload := rf.payload("")
	rf.create(t, payload)
	sess := rf.session()
	stale := time.Now().Add(-nzbSessionTTL / 2)
	nzbSessionsMu.Lock()
	sess.lastAccess = stale
	nzbSessionsMu.Unlock()

	rf.create(t, payload)
	if rf.session() != sess {
		t.Fatal("session was not reused")
	}
	nzbSessionsMu.Lock()
	last, refs, noReuse, builds := sess.lastAccess, sess.refCount, sess.noReuse, len(nzbBuilds)
	nzbSessionsMu.Unlock()
	if !last.After(stale) {
		t.Error("reuse did not refresh lastAccess")
	}
	if refs != 0 {
		t.Errorf("refCount = %d after the create returned, want 0", refs)
	}
	if noReuse {
		t.Error("a healthy session was marked failed")
	}
	if builds != 0 {
		t.Errorf("%d build flights left registered", builds)
	}
}

// A replaced session that is still pinned (a request is reading it) is parked
// as an orphan and retired by the last release: no leak, nothing left behind.
func TestNzbCreate_ReplacedPinnedSessionIsRetiredByLastRelease(t *testing.T) {
	rf := newNzbReuseFixture(t, 4, 4_000, 0, 2)
	rf.create(t, rf.payload(""))
	old := rf.session()
	nzbSessionsMu.Lock()
	old.refCount++ // a request is reading it ...
	nzbSessionsMu.Unlock()
	rf.create(t, fmt.Sprintf(`{"nzbUrl":%q,"servers":[%q]}`, rf.nzbURL+"?v=2", rf.nntp)) // ... when another payload replaces it
	nzbSessionsMu.Lock()
	_, orphan := nzbOrphans[old]
	nzbSessionsMu.Unlock()
	if !orphan {
		t.Fatal("pinned replaced session must be parked as an orphan")
	}
	nzbRelease(old)
	waitGone(t, old.tmpDir)
	nzbSessionsMu.Lock()
	_, orphan = nzbOrphans[old]
	nzbSessionsMu.Unlock()
	if orphan {
		t.Error("orphan not retired by the last release")
	}
}
