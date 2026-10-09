// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package dvr

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProc is a controllable recorder: it creates the output file when
// started, runs until Quit/Kill/finish, and reports a scripted exit.
type fakeProc struct {
	out        string
	stderr     string
	ignoreQuit bool

	quitOnce sync.Once
	quit     chan struct{}
	killed   chan struct{}
	done     chan error
	mu       sync.Mutex
	quits    int
	kills    int
}

func (p *fakeProc) Wait() error {
	select {
	case err := <-p.done:
		return err
	case <-p.quit:
		return nil
	case <-p.killed:
		return errors.New("signal: killed")
	}
}

func (p *fakeProc) Quit() {
	p.mu.Lock()
	p.quits++
	p.mu.Unlock()
	if !p.ignoreQuit {
		p.quitOnce.Do(func() { close(p.quit) })
	}
}

func (p *fakeProc) Kill() {
	p.mu.Lock()
	p.kills++
	p.mu.Unlock()
	select {
	case <-p.killed:
	default:
		close(p.killed)
	}
}

func (p *fakeProc) Output() string { return p.stderr }

func (p *fakeProc) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.quits, p.kills
}

type fakeRunner struct {
	mu     sync.Mutex
	procs  []*fakeProc
	args   [][]string
	bins   []string
	data   string // written to the output file at start ("" = empty file)
	stderr string
	ignore bool
	err    error
}

func (r *fakeRunner) start(bin string, args []string) (Proc, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	out := args[len(args)-1]
	if err := os.WriteFile(out, []byte(r.data), 0o600); err != nil {
		return nil, err
	}
	p := &fakeProc{out: out, stderr: r.stderr, ignoreQuit: r.ignore,
		quit: make(chan struct{}), killed: make(chan struct{}), done: make(chan error, 1)}
	r.procs = append(r.procs, p)
	r.args = append(r.args, args)
	r.bins = append(r.bins, bin)
	return p, nil
}

func (r *fakeRunner) last() (*fakeProc, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.procs[len(r.procs)-1], r.args[len(r.args)-1]
}

func newMgr(t *testing.T, r *fakeRunner, mutate func(*Config)) *Manager {
	t.Helper()
	cfg := Config{
		Dir:             t.TempDir(),
		DefaultDuration: time.Hour,
		MaxDuration:     2 * time.Hour,
		MaxActive:       2,
		StopGrace:       time.Second,
		Start:           r.start,
		LookPath:        func(string) (string, error) { return "/usr/bin/ffmpeg", nil },
	}
	if mutate != nil {
		mutate(&cfg)
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func arg(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSanitizeName(t *testing.T) {
	cases := []struct{ in, display, stem string }{
		{"Sky Sport F1", "Sky Sport F1", "Sky_Sport_F1"},
		{"../../etc/passwd", "../../etc/passwd", "etcpasswd"},
		{`a/b\c:d*e?f"g<h>i|j`, `a/b\c:d*e?f"g<h>i|j`, "abcdefghij"},
		{"..", "..", "recording"},
		{"", "", "recording"},
		{"   ", "", "recording"},
		{"tab\tnew\nline\x00nul", "tabnewlinenul", "tabnewlinenul"},
		{"Città — è l'été", "Città — è l'été", "Città__è_lété"},
		{strings.Repeat("x", 300), strings.Repeat("x", 100), strings.Repeat("x", 50)},
		{"-_-", "-_-", "-_-"},
	}
	for _, c := range cases {
		d, s := SanitizeName(c.in)
		if d != c.display || s != c.stem {
			t.Errorf("SanitizeName(%q) = (%q, %q); want (%q, %q)", c.in, d, s, c.display, c.stem)
		}
		if strings.ContainsAny(s, `/\.`) {
			t.Errorf("stem %q contains a path character", s)
		}
	}
}

func TestStartContainsFileInDirForHostileName(t *testing.T) {
	r := &fakeRunner{data: "x"}
	m := newMgr(t, r, nil)
	for _, name := range []string{"../../../../tmp/evil", "/etc/cron.d/x", `..\..\win`, "a\x00b", "../"} {
		rec, err := m.Start(StartRequest{Name: name, Source: "http://s/" + name, Input: "http://127.0.0.1/p"})
		if err != nil {
			t.Fatalf("Start(%q): %v", name, err)
		}
		_, args := r.last()
		out := args[len(args)-1]
		if filepath.Dir(out) != m.Dir() {
			t.Errorf("name %q: output %q escapes %q", name, out, m.Dir())
		}
		if filepath.Base(out) != rec.FilePath || strings.ContainsAny(rec.FilePath, `/\`) || !strings.HasSuffix(rec.FilePath, ".ts") {
			t.Errorf("name %q: bad file_path %q", name, rec.FilePath)
		}
		if _, err := m.Stop(rec.ID); err != nil {
			t.Fatal(err)
		}
	}
	// Nothing was created outside the recordings directory.
	ents, _ := os.ReadDir(filepath.Dir(m.Dir()))
	for _, e := range ents {
		if e.Name() != filepath.Base(m.Dir()) {
			t.Errorf("unexpected sibling %q next to the recordings dir", e.Name())
		}
	}
}

func TestValidID(t *testing.T) {
	for _, id := range []string{"", "..", "../x", "20260101_000000_abcdef01/..", "20260101_000000_ABCDEF01", "x"} {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true", id)
		}
	}
	if !ValidID("20260101_235959_0123abcd") {
		t.Error("generated id shape rejected")
	}
	m := newMgr(t, &fakeRunner{}, nil)
	if _, ok := m.Get("../../etc/passwd"); ok {
		t.Error("Get accepted a traversal id")
	}
	if _, _, err := m.Open("../../etc/passwd"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Open traversal id: %v", err)
	}
	if err := m.Delete("../../etc/passwd"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete traversal id: %v", err)
	}
}

func TestClampDurationAndArgs(t *testing.T) {
	r := &fakeRunner{data: "x"}
	m := newMgr(t, r, func(c *Config) { c.MaxActive = 10 })
	cases := []struct {
		req  time.Duration
		want time.Duration
	}{
		{0, time.Hour},                // default
		{-5 * time.Second, time.Hour}, // non-positive -> default
		{90 * time.Second, 90 * time.Second},
		{2 * time.Hour, 2 * time.Hour},
		{9999 * time.Hour, 2 * time.Hour}, // capped at MaxDuration
	}
	for i, c := range cases {
		if got := m.ClampDuration(c.req); got != c.want {
			t.Errorf("ClampDuration(%v) = %v; want %v", c.req, got, c.want)
		}
		rec, err := m.Start(StartRequest{Name: "d", Source: "http://s/" + string(rune('a'+i)), Input: "http://127.0.0.1/p", Duration: c.req})
		if err != nil {
			t.Fatal(err)
		}
		_, args := r.last()
		if got, want := arg(args, "-t"), formatSecs(c.want); got != want {
			t.Errorf("duration %v: ffmpeg -t %s; want %s", c.req, got, want)
		}
		if rec.MaxDurationSecs != int64(c.want/time.Second) {
			t.Errorf("max_duration_seconds = %d; want %d", rec.MaxDurationSecs, int64(c.want/time.Second))
		}
	}
	// Default can never exceed the cap.
	m2 := newMgr(t, r, func(c *Config) { c.DefaultDuration = 10 * time.Hour; c.MaxDuration = time.Hour })
	if got := m2.ClampDuration(0); got != time.Hour {
		t.Errorf("default above cap = %v", got)
	}
}

func formatSecs(d time.Duration) string { return strconv.FormatInt(int64(d/time.Second), 10) }

func TestBuildArgs(t *testing.T) {
	args := BuildArgs("http://127.0.0.1:1/proxy/hls/manifest.m3u8?d=x", "/rec/a.ts", 90*time.Second, 5000)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-protocol_whitelist http,https,tcp,tls,crypto", "-i http://127.0.0.1:1/proxy/hls/manifest.m3u8?d=x",
		"-t 90", "-c copy", "-fs 5000", "-f mpegts /rec/a.ts", "-map 0:v:0? -map 0:a:0?",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %s", want, joined)
		}
	}
	if strings.Contains(strings.Join(BuildArgs("u", "o", time.Second, 0), " "), "-fs") {
		t.Error("-fs set without a quota")
	}
}

func TestLifecycleListStopDelete(t *testing.T) {
	r := &fakeRunner{data: "TSDATA"}
	m := newMgr(t, r, nil)
	rec, err := m.Start(StartRequest{Name: "Match", Source: "https://src/live.m3u8?api_password=secret&x=1", Input: "http://127.0.0.1:1/in?api_password=secret"})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != StatusRecording || !rec.IsActive || rec.ElapsedSeconds == nil || rec.Name != "Match" {
		t.Fatalf("bad start view: %+v", rec)
	}
	if rec.URL != "https://src/live.m3u8?x=1" {
		t.Errorf("stored url not redacted: %q", rec.URL)
	}
	if got := arg(r.args[0], "-i"); got != "http://127.0.0.1:1/in?api_password=secret" {
		t.Errorf("ffmpeg input = %q; must be the unredacted loopback URL", got)
	}
	if got := m.Active(); len(got) != 1 || got[0].ID != rec.ID {
		t.Fatalf("Active = %+v", got)
	}
	if got := m.List("recording"); len(got) != 1 {
		t.Fatalf("List(recording) = %d", len(got))
	}
	if got := m.List("stopped"); len(got) != 0 {
		t.Fatalf("List(stopped) = %d", len(got))
	}

	p, _ := r.last()
	stopped, err := m.Stop(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if q, k := p.counts(); q != 1 || k != 0 {
		t.Errorf("graceful stop: quits=%d kills=%d", q, k)
	}
	if stopped.Status != StatusStopped || stopped.IsActive || stopped.StoppedAt == nil ||
		stopped.DurationSeconds == nil || stopped.FileSizeBytes == nil || *stopped.FileSizeBytes != 6 {
		t.Fatalf("bad stopped view: %+v", stopped)
	}
	if _, err := m.Stop(rec.ID); !errors.Is(err, ErrNotActive) {
		t.Errorf("second Stop: %v", err)
	}
	if _, err := m.Stop("20260101_000000_00000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stop unknown: %v", err)
	}

	f, _, err := m.Open(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := os.Stat(filepath.Join(m.Dir(), rec.ID+".json")); err != nil {
		t.Errorf("sidecar missing: %v", err)
	}

	if err := m.Delete(rec.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(rec.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete: %v", err)
	}
	ents, _ := os.ReadDir(m.Dir())
	if len(ents) != 0 {
		t.Errorf("delete left files behind: %v", ents)
	}
}

func TestDeleteActiveStopsFirst(t *testing.T) {
	r := &fakeRunner{data: "x"}
	m := newMgr(t, r, nil)
	rec, _ := m.Start(StartRequest{Name: "a", Source: "http://s/a", Input: "http://i/a"})
	p, _ := r.last()
	if err := m.Delete(rec.ID); err != nil {
		t.Fatal(err)
	}
	if q, _ := p.counts(); q != 1 {
		t.Errorf("recorder not asked to quit before delete (quits=%d)", q)
	}
	if ents, _ := os.ReadDir(m.Dir()); len(ents) != 0 {
		t.Errorf("files left: %v", ents)
	}
}

func TestDeleteAllWithStatusFilter(t *testing.T) {
	r := &fakeRunner{data: "x"}
	m := newMgr(t, r, nil)
	a, _ := m.Start(StartRequest{Name: "a", Source: "http://s/a", Input: "i"})
	_, _ = m.Stop(a.ID)
	_, _ = m.Start(StartRequest{Name: "b", Source: "http://s/b", Input: "i"})
	if n := m.DeleteAll("stopped"); n != 1 {
		t.Errorf("DeleteAll(stopped) = %d", n)
	}
	if n := m.DeleteAll(""); n != 1 {
		t.Errorf("DeleteAll(all) = %d", n)
	}
	if len(m.List("")) != 0 {
		t.Error("recordings remain")
	}
}

func TestDuplicateSource(t *testing.T) {
	r := &fakeRunner{data: "x"}
	m := newMgr(t, r, nil)
	first, err := m.Start(StartRequest{Name: "a", Source: "http://s/same?api_password=a", Input: "i"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := m.Start(StartRequest{Name: "a", Source: "http://s/same?api_password=b", Input: "i"})
	if !errors.Is(err, ErrDuplicate) || again.ID != first.ID {
		t.Fatalf("want ErrDuplicate with the running recording, got %v / %+v", err, again)
	}
	if len(r.procs) != 1 {
		t.Errorf("a second recorder was spawned (%d)", len(r.procs))
	}
}

func TestActiveCap(t *testing.T) {
	r := &fakeRunner{data: "x"}
	m := newMgr(t, r, func(c *Config) { c.MaxActive = 1 })
	a, err := m.Start(StartRequest{Name: "a", Source: "http://s/a", Input: "i"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(StartRequest{Name: "b", Source: "http://s/b", Input: "i"}); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	if _, err := m.Stop(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(StartRequest{Name: "b", Source: "http://s/b", Input: "i"}); err != nil {
		t.Fatalf("slot not released after stop: %v", err)
	}
}

func TestMissingFFmpeg(t *testing.T) {
	r := &fakeRunner{}
	m := newMgr(t, r, func(c *Config) {
		c.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	})
	if m.FFmpegAvailable() {
		t.Error("FFmpegAvailable with no binary")
	}
	if _, err := m.Start(StartRequest{Name: "a", Source: "http://s/a", Input: "i"}); !errors.Is(err, ErrNoFFmpeg) {
		t.Fatalf("want ErrNoFFmpeg, got %v", err)
	}
	if ents, _ := os.ReadDir(m.Dir()); len(ents) != 0 {
		t.Errorf("files created without ffmpeg: %v", ents)
	}
}

func TestStorageQuota(t *testing.T) {
	r := &fakeRunner{data: "0123456789"}
	m := newMgr(t, r, func(c *Config) { c.MaxBytes = 25; c.MaxActive = 5 })
	a, err := m.Start(StartRequest{Name: "a", Source: "http://s/a", Input: "i"})
	if err != nil {
		t.Fatal(err)
	}
	_, args := r.last()
	if got := arg(args, "-fs"); got != "25" {
		t.Errorf("-fs on an empty store = %q; want 25", got)
	}
	_, _ = m.Stop(a.ID)
	b, err := m.Start(StartRequest{Name: "b", Source: "http://s/b", Input: "i"})
	if err != nil {
		t.Fatal(err)
	}
	_, args = r.last()
	if got := arg(args, "-fs"); got != "15" {
		t.Errorf("-fs with 10 bytes used of 25 = %q; want 15", got)
	}
	_, _ = m.Stop(b.ID)
	// 10 (a) + 20 (b) bytes now exceed the 25-byte quota.
	if err := os.WriteFile(filepath.Join(m.Dir(), b.FilePath), []byte(strings.Repeat("x", 20)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(StartRequest{Name: "c", Source: "http://s/c", Input: "i"}); !errors.Is(err, ErrDiskFull) {
		t.Fatalf("want ErrDiskFull, got %v", err)
	}
}

func TestNaturalCompletion(t *testing.T) {
	r := &fakeRunner{data: "DATA"}
	m := newMgr(t, r, nil)
	rec, _ := m.Start(StartRequest{Name: "a", Source: "http://s/a", Input: "i"})
	p, _ := r.last()
	p.done <- nil
	waitFor(t, "completed", func() bool { v, _ := m.Get(rec.ID); return v.Status == StatusCompleted })
	v, _ := m.Get(rec.ID)
	if v.IsActive || v.FileSizeBytes == nil || *v.FileSizeBytes != 4 || v.StoppedAt == nil {
		t.Errorf("bad completed view: %+v", v)
	}
}

func TestFailureKeepsErrorRedactedAndDropsEmptyFile(t *testing.T) {
	r := &fakeRunner{data: "", stderr: "Server returned 502 for http://127.0.0.1:1/proxy/hls/manifest.m3u8?d=abc&api_password=hunter2&h_Authorization=Bearer%20tok&token=t0k3n"}
	m := newMgr(t, r, nil)
	rec, _ := m.Start(StartRequest{Name: "bad", Source: "http://s/bad", Input: "i"})
	p, _ := r.last()
	p.done <- errors.New("exit status 1")
	waitFor(t, "failed", func() bool { v, _ := m.Get(rec.ID); return v.Status == StatusFailed })
	v, _ := m.Get(rec.ID)
	if v.ErrorMessage == nil || !strings.Contains(*v.ErrorMessage, "exit status 1") || !strings.Contains(*v.ErrorMessage, "502") {
		t.Fatalf("error_message = %v", v.ErrorMessage)
	}
	for _, leak := range []string{"hunter2", "t0k3n", "Bearer"} {
		if strings.Contains(*v.ErrorMessage, leak) {
			t.Errorf("error_message leaks %q: %s", leak, *v.ErrorMessage)
		}
	}
	if _, err := os.Stat(filepath.Join(m.Dir(), rec.FilePath)); !os.IsNotExist(err) {
		t.Errorf("empty partial file was kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.Dir(), rec.ID+".json")); err != nil {
		t.Errorf("failure row not persisted: %v", err)
	}
}

func TestStartFailureCleansUp(t *testing.T) {
	r := &fakeRunner{err: errors.New("boom")}
	m := newMgr(t, r, nil)
	if _, err := m.Start(StartRequest{Name: "a", Source: "http://s/a", Input: "i"}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if ents, _ := os.ReadDir(m.Dir()); len(ents) != 0 {
		t.Errorf("left files after failed start: %v", ents)
	}
	if len(m.List("")) != 0 {
		t.Error("failed start left a recording")
	}
}

func TestWallClockCap(t *testing.T) {
	r := &fakeRunner{data: "x"}
	m := newMgr(t, r, func(c *Config) { c.Overrun = 20 * time.Millisecond })
	rec, _ := m.Start(StartRequest{Name: "a", Source: "http://s/a", Input: "i", Duration: time.Second})
	waitFor(t, "cap", func() bool { v, _ := m.Get(rec.ID); return !v.IsActive })
	v, _ := m.Get(rec.ID)
	if v.Status != StatusCompleted {
		t.Errorf("status after wall-clock cap = %s; want completed", v.Status)
	}
	p, _ := r.last()
	if q, _ := p.counts(); q != 1 {
		t.Errorf("recorder not quit at the cap (quits=%d)", q)
	}
}

func TestStopKillsStuckRecorder(t *testing.T) {
	r := &fakeRunner{data: "x", ignore: true}
	m := newMgr(t, r, func(c *Config) { c.StopGrace = 30 * time.Millisecond })
	rec, _ := m.Start(StartRequest{Name: "a", Source: "http://s/a", Input: "i"})
	v, err := m.Stop(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := r.last()
	if q, k := p.counts(); q != 1 || k != 1 {
		t.Errorf("quits=%d kills=%d; want 1/1", q, k)
	}
	if v.Status != StatusStopped {
		t.Errorf("status = %s", v.Status)
	}
}

func TestCloseStopsActiveRecordingsGracefully(t *testing.T) {
	r := &fakeRunner{data: "x"}
	m := newMgr(t, r, nil)
	a, _ := m.Start(StartRequest{Name: "a", Source: "http://s/a", Input: "i"})
	b, _ := m.Start(StartRequest{Name: "b", Source: "http://s/b", Input: "i"})
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a.ID, b.ID} {
		v, _ := m.Get(id)
		if v.Status != StatusStopped || v.IsActive {
			t.Errorf("%s after Close: %+v", id, v)
		}
		if _, err := os.Stat(filepath.Join(m.Dir(), v.FilePath)); err != nil {
			t.Errorf("recording file dropped on shutdown: %v", err)
		}
	}
	for _, p := range r.procs {
		if q, k := p.counts(); q != 1 || k != 0 {
			t.Errorf("shutdown quits=%d kills=%d", q, k)
		}
	}
	if _, err := m.Start(StartRequest{Name: "c", Source: "http://s/c", Input: "i"}); !errors.Is(err, ErrClosed) {
		t.Errorf("Start after Close: %v", err)
	}
	if err := m.Close(); err != nil { // idempotent
		t.Fatal(err)
	}
}

func TestRestartRecoversInterruptedRecordings(t *testing.T) {
	dir := t.TempDir()
	mk := func(id, stem, data string) {
		file := id + "_" + stem + ".ts"
		if data != "" {
			if err := os.WriteFile(filepath.Join(dir, file), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		b, _ := json.Marshal(stored{ID: id, Name: stem, URL: "http://s/" + id, File: file, Status: StatusRecording,
			StartedAt: time.Now().Add(-time.Minute), MaxDuration: 60})
		if err := os.WriteFile(filepath.Join(dir, id+".json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mk("20260101_000000_aaaaaaaa", "kept", "PARTIAL")
	mk("20260101_000001_bbbbbbbb", "empty", "")
	_ = os.WriteFile(filepath.Join(dir, ".rec-123.tmp"), []byte("{"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "garbage.json"), []byte("{"), 0o600)
	// A sidecar naming a file outside the directory must not be honoured.
	evil, _ := json.Marshal(stored{ID: "20260101_000002_cccccccc", Name: "x", File: "../outside.ts", Status: StatusCompleted, StartedAt: time.Now()})
	_ = os.WriteFile(filepath.Join(dir, "20260101_000002_cccccccc.json"), evil, 0o600)

	m, err := New(Config{Dir: dir, LookPath: func(string) (string, error) { return "ffmpeg", nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	kept, ok := m.Get("20260101_000000_aaaaaaaa")
	if !ok || kept.Status != StatusFailed || kept.IsActive || kept.FileSizeBytes == nil || *kept.FileSizeBytes != 7 ||
		kept.ErrorMessage == nil || !strings.Contains(*kept.ErrorMessage, "restart") {
		t.Fatalf("interrupted recording with data: %+v", kept)
	}
	empty, ok := m.Get("20260101_000001_bbbbbbbb")
	if !ok || empty.Status != StatusFailed || *empty.FileSizeBytes != 0 {
		t.Fatalf("interrupted empty recording: %+v", empty)
	}
	if _, ok := m.Get("20260101_000002_cccccccc"); ok {
		t.Error("sidecar with an escaping file name was loaded")
	}
	if _, err := os.Stat(filepath.Join(dir, ".rec-123.tmp")); !os.IsNotExist(err) {
		t.Error("stray temp file survived startup")
	}
	if _, err := m.Stop(kept.ID); !errors.Is(err, ErrNotActive) {
		t.Errorf("recovered recording is stoppable: %v", err)
	}
}

func TestRetentionSweep(t *testing.T) {
	r := &fakeRunner{data: "x"}
	now := time.Now()
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	m := newMgr(t, r, func(c *Config) { c.Retention = 24 * time.Hour; c.Now = clock; c.SweepEvery = time.Hour })
	old, _ := m.Start(StartRequest{Name: "old", Source: "http://s/old", Input: "i"})
	_, _ = m.Stop(old.ID)
	live, _ := m.Start(StartRequest{Name: "live", Source: "http://s/live", Input: "i"})
	mu.Lock()
	now = now.Add(48 * time.Hour)
	mu.Unlock()
	m.sweep()
	if _, ok := m.Get(old.ID); ok {
		t.Error("expired recording kept")
	}
	if _, err := os.Stat(filepath.Join(m.Dir(), old.FilePath)); !os.IsNotExist(err) {
		t.Error("expired file kept")
	}
	if v, ok := m.Get(live.ID); !ok || !v.IsActive {
		t.Error("active recording must never be aged out")
	}
}

func TestRedactURL(t *testing.T) {
	cases := map[string]string{
		"https://h/p.m3u8":                            "https://h/p.m3u8",
		"https://h/p?api_password=x":                  "https://h/p",
		"https://h/p?a=1&api_password=x&b=2":          "https://h/p?a=1&b=2",
		"https://h/p?token=abc&d=ZGVm#frag":           "https://h/p?d=ZGVm#frag",
		"https://h/p?a=1&api%5Fpassword=x":            "https://h/p?a=1",
		"https://h/p?key=api_password&notoken=1":      "https://h/p?key=api_password&notoken=1",
		"https://h/p?d=http%3A%2F%2Fa%2Fb%3Ftoken%3D": "https://h/p?d=http%3A%2F%2Fa%2Fb%3Ftoken%3D",
	}
	for in, want := range cases {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q; want %q", in, got, want)
		}
	}
}

// TestExecRunnerWithFakeFFmpeg drives the real os/exec runner with a shell
// script standing in for ffmpeg: it must receive the argument vector, write
// the output file, and exit cleanly on the 'q' / stdin-close shutdown request.
func TestExecRunnerWithFakeFFmpeg(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "args.txt")
	script := filepath.Join(dir, "fake-ffmpeg")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsLog + "\nfor a; do out=\"$a\"; done\nprintf 'TSDATA' > \"$out\"\ncat >/dev/null\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := New(Config{Dir: filepath.Join(dir, "rec"), FFmpeg: script, StopGrace: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if !m.FFmpegAvailable() {
		t.Fatal("script not found")
	}
	rec, err := m.Start(StartRequest{Name: "Exec Test", Source: "http://s/x", Input: "http://127.0.0.1:1/proxy/stream?d=x", Duration: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "args file", func() bool { _, err := os.Stat(argsLog); return err == nil })
	waitFor(t, "output file", func() bool {
		v, _ := m.Get(rec.ID)
		return v.FileSizeBytes != nil && *v.FileSizeBytes == 6
	})
	got, _ := os.ReadFile(argsLog)
	if !strings.Contains(string(got), "http://127.0.0.1:1/proxy/stream?d=x") || !strings.Contains(string(got), "\n30\n") {
		t.Errorf("fake ffmpeg args: %s", got)
	}
	v, err := m.Stop(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != StatusStopped || v.FileSizeBytes == nil || *v.FileSizeBytes != 6 {
		t.Errorf("after stop: %+v", v)
	}
}
