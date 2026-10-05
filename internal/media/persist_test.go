// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package media

// Tests for opt-in HLS session persistence (persist.go). Offline and
// ffmpeg-free: managers are built without newHLS (which probes encoders),
// ffprobe is replaced through the hlsManager.probe seam, and "transcoded"
// segments are plain files written into the session directory, which
// transcodeSegment serves from cache without spawning ffmpeg.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/types"
)

// testPersistURL is a public IP literal: it passes validateRemoteURL and
// validatePersistedURL without any DNS lookup, and is never actually fetched.
const testPersistURL = "http://93.184.216.34/movie.mkv"

// newTestPersistManager builds a manager the way newHLS does with Persist on
// (newHLSBase + loadPersisted), minus selectEncoder and the reaper goroutine.
// It holds the persist dir lock until CloseHLS (registered as cleanup too),
// so tests close one manager before building the next on the same dir.
func newTestPersistManager(t *testing.T, workDir string, cfg HLSConfig, settings SettingsSource) *hlsManager {
	t.Helper()
	cfg.WorkDir = workDir
	cfg.Persist = true
	cfg = cfg.normalize(1)
	base, persist, lock := newHLSBase(cfg)
	if !persist {
		t.Fatalf("newHLSBase(Persist, WorkDir=%q) did not enable persistence", workDir)
	}
	if want := filepath.Join(workDir, persistDirName); base != want {
		t.Fatalf("persist base = %q, want %q", base, want)
	}
	m := &hlsManager{
		base:        base,
		cfg:         cfg,
		settings:    settings,
		sessions:    map[string]*hlsSession{},
		probeCache:  map[string]probeCacheEntry{},
		stopCh:      make(chan struct{}),
		persist:     true,
		persistLock: lock,
	}
	t.Cleanup(m.CloseHLS)
	m.loadPersisted()
	return m
}

// seedPersistedSession registers a probed session on m and writes its
// session.json, exactly as StartHLS does after a successful probe.
func seedPersistedSession(t *testing.T, m *hlsManager, id string, lastAccess time.Time) string {
	t.Helper()
	return seedPersistedSessionWith(t, m, id, lastAccess, nil)
}

// seedPersistedSessionWith is seedPersistedSession with mut applied to the
// session before it is persisted.
func seedPersistedSessionWith(t *testing.T, m *hlsManager, id string, lastAccess time.Time, mut func(*hlsSession)) string {
	t.Helper()
	dir := filepath.Join(m.base, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s := &hlsSession{
		mediaURL:     testPersistURL,
		dir:          dir,
		duration:     20,
		segLocks:     map[string]*sync.Mutex{},
		playlistData: map[string]struct{}{},
		tc:           m.effectiveSessionConfig(sessionOverrides{}),
	}
	if mut != nil {
		mut(s)
	}
	s.lastAccess.Store(lastAccess.UnixNano())
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	m.persistSession(id, s)
	if _, err := os.Stat(filepath.Join(dir, sessionFileName)); err != nil {
		t.Fatalf("seed %s: session.json not written: %v", id, err)
	}
	return dir
}

func fakeProbeResult() probeMediaResult {
	return probeMediaResult{
		duration: 20,
		audioStreams: []audioStream{
			{Index: 1, CodecName: "aac", Channels: 2, Language: "eng", IsDefault: true},
		},
		subtitleStreams: []subtitleStream{
			{Index: 2, SubIdx: 0, CodecName: "subrip", Language: "ita", Title: "Italiano"},
		},
		highBitDepth: true,
		width:        640,
		height:       360,
	}
}

func fakeProbe(context.Context, string, string, time.Duration) probeMediaResult {
	return fakeProbeResult()
}

// captureLogs routes the default slog logger (which logging.For wraps) into
// a buffer for the duration of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return &buf
}

func assertGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("%s still exists (err=%v), want removed", path, err)
	}
}

func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("%s missing: %v", path, err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" { // no POSIX mode bits
		return
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Errorf("stat %s: %v", path, err)
		return
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("%s mode = %04o, want %04o", path, got, want)
	}
}

// symlinkOrSkip creates a symlink, skipping the test where that needs
// privileges the test process lacks (e.g. Windows without developer mode).
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

// TestHLSPersistOffUnchanged: with Persist off, newHLSBase is exactly
// newHLSBaseDir (a fresh random stremio-hls-* dir, under WorkDir when set)
// and CloseHLS still deletes it.
func TestHLSPersistOffUnchanged(t *testing.T) {
	for _, workDir := range []string{"", t.TempDir()} {
		base, persist, lock := newHLSBase(HLSConfig{WorkDir: workDir})
		if persist || lock != nil {
			t.Fatalf("WorkDir=%q: persist enabled without Persist", workDir)
		}
		if !strings.HasPrefix(filepath.Base(base), "stremio-hls-") || filepath.Base(base) == persistDirName {
			t.Errorf("WorkDir=%q: base = %q, want a random stremio-hls-* dir", workDir, base)
		}
		if workDir != "" && filepath.Dir(base) != workDir {
			t.Errorf("base %q not under WorkDir %q", base, workDir)
		}
		m := &hlsManager{base: base, cfg: DefaultHLSConfig().normalize(1), sessions: map[string]*hlsSession{}, stopCh: make(chan struct{})}
		m.CloseHLS()
		assertGone(t, base)
	}
}

// TestHLSPersistWithoutWorkDirWarns: Persist without WorkDir logs a warning
// and falls back to today's behaviour.
func TestHLSPersistWithoutWorkDirWarns(t *testing.T) {
	logs := captureLogs(t)
	base, persist, _ := newHLSBase(HLSConfig{Persist: true})
	defer os.RemoveAll(base)
	if persist {
		t.Fatal("persistence enabled without WorkDir")
	}
	if !strings.HasPrefix(filepath.Base(base), "stremio-hls-") || filepath.Base(base) == persistDirName {
		t.Errorf("base = %q, want a random stremio-hls-* dir", base)
	}
	if got := logs.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "STREMIO_HLS_PERSIST requires STREMIO_HLS_WORK_DIR") {
		t.Errorf("missing warning; logs:\n%s", got)
	}
}

// TestHLSPersistDefaultTTLWarns: persistence with the default 60s session
// TTL is almost useless, so it is called out at startup.
func TestHLSPersistDefaultTTLWarns(t *testing.T) {
	for _, tc := range []struct {
		ttl  time.Duration
		warn bool
	}{{0, true}, {defaultSessionTTL, true}, {12 * time.Hour, false}} {
		logs := captureLogs(t)
		cfg := HLSConfig{WorkDir: t.TempDir(), Persist: true, SessionTTL: tc.ttl}.normalize(1)
		_, persist, lock := newHLSBase(cfg)
		if !persist {
			t.Fatalf("ttl=%s: persistence not enabled", tc.ttl)
		}
		_ = lock.Close()
		if got := strings.Contains(logs.String(), "STREMIO_HLS_SESSION_TTL is still the default"); got != tc.warn {
			t.Errorf("ttl=%s: default-TTL warning = %v, want %v; logs:\n%s", tc.ttl, got, tc.warn, logs)
		}
	}
}

// TestHLSPersistRoundTrip: a session created (and probed) by one manager is
// restored by the next one without probing, and a segment already on disk
// is served from cache.
func TestHLSPersistRoundTrip(t *testing.T) {
	work := t.TempDir()
	cfg := HLSConfig{SegmentTimeout: time.Second} // bounds any accidental ffmpeg run

	m1 := newTestPersistManager(t, work, cfg, nil)
	var probes atomic.Int32
	m1.probe = func(context.Context, string, string, time.Duration) probeMediaResult {
		probes.Add(1)
		return fakeProbeResult()
	}
	master1, err := m1.StartHLS("sess-1", testPersistURL, types.HLSSessionOptions{})
	if err != nil {
		t.Fatalf("StartHLS: %v", err)
	}
	if probes.Load() != 1 {
		t.Fatalf("probes = %d, want 1", probes.Load())
	}

	dir := filepath.Join(work, persistDirName, "sess-1")
	jsonPath := filepath.Join(dir, sessionFileName)
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("session.json not written at creation: %v", err)
	}
	var rec persistedSession
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Version != sessionFormatVersion || rec.ID != "sess-1" || rec.MediaURL != testPersistURL ||
		rec.Probe.Duration != 20 || rec.Probe.Width != 640 || !rec.Probe.HighBitDepth ||
		len(rec.Probe.AudioStreams) != 1 || len(rec.Probe.SubtitleStreams) != 1 ||
		rec.LastAccess.IsZero() || rec.Fingerprint == "" {
		t.Errorf("unexpected session.json: %s", data)
	}
	assertMode(t, jsonPath, 0o600)
	assertMode(t, dir, 0o700)
	assertMode(t, filepath.Join(work, persistDirName), 0o700)

	// "Transcode" seg0 and leave a partial seg1 behind, as a killed ffmpeg would.
	if err := os.WriteFile(filepath.Join(dir, "seg0.ts"), []byte("segment-zero"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "seg1.ts.tmp.ts"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	m1.CloseHLS()
	assertExists(t, jsonPath)

	m2 := newTestPersistManager(t, work, cfg, nil)
	m2.probe = func(context.Context, string, string, time.Duration) probeMediaResult {
		t.Error("restored session was re-probed")
		return probeMediaResult{}
	}
	if n := m2.Sessions(); n != 1 {
		t.Fatalf("Sessions() after restore = %d, want 1", n)
	}
	assertGone(t, filepath.Join(dir, "seg1.ts.tmp.ts"))

	master2, err := m2.StartHLS("sess-1", testPersistURL, types.HLSSessionOptions{})
	if err != nil {
		t.Fatalf("StartHLS on restored session: %v", err)
	}
	if master2 != master1 {
		t.Errorf("master playlist changed across restart:\n%s\nvs\n%s", master1, master2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, ctype, err := m2.HLSFile(ctx, "sess-1", "seg0.ts")
	if err != nil {
		t.Fatalf("HLSFile(seg0.ts): %v", err)
	}
	if got, _ := os.ReadFile(p); string(got) != "segment-zero" || ctype != "video/mp2t" {
		t.Errorf("seg0.ts = %q (%s), want the cached segment", got, ctype)
	}
	if _, _, err := m2.HLSFile(ctx, "sess-1", "playlist.m3u8"); err != nil {
		t.Errorf("HLSFile(playlist.m3u8) on restored session: %v", err)
	}

	m2.mu.Lock()
	s := m2.sessions["sess-1"]
	m2.mu.Unlock()
	if s.tc != m2.effectiveSessionConfig(sessionOverrides{}) || !s.highBitDepth || s.srcWidth != 640 || s.srcHeight != 360 || s.multiAudio {
		t.Errorf("restored session state mismatch: %+v", s)
	}
}

// TestHLSPersistSurvivesSettingsChange is the reviewer's scenario: a
// /settings change between runs must not discard a session, which keeps
// encoding with its own snapshot exactly as it would have in-process.
func TestHLSPersistSurvivesSettingsChange(t *testing.T) {
	work := t.TempDir()
	settings := stubSettings{}
	m1 := newTestPersistManager(t, work, HLSConfig{}, settings)
	m1.probe = fakeProbe
	master1, err := m1.StartHLS("sess-s", testPersistURL, types.HLSSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	orig := m1.effectiveSessionConfig(sessionOverrides{})

	settings["transcodeMaxBitRate"] = float64(3_000_000)
	settings["transcodeProfile"] = "slow"
	m1.CloseHLS()

	m2 := newTestPersistManager(t, work, HLSConfig{}, settings)
	m2.mu.Lock()
	s, ok := m2.sessions["sess-s"]
	m2.mu.Unlock()
	if !ok {
		t.Fatal("session discarded after a /settings change")
	}
	if s.tc != orig {
		t.Errorf("restored tc = %+v, want the original snapshot %+v", s.tc, orig)
	}
	if s.tc == m2.effectiveSessionConfig(sessionOverrides{}) {
		t.Error("test is vacuous: settings change did not alter the effective config")
	}
	if master2, err := m2.StartHLS("sess-s", testPersistURL, types.HLSSessionOptions{}); err != nil || master2 != master1 {
		t.Errorf("master after restart = %q, %v; want %q", master2, err, master1)
	}
}

// TestHLSPersistFlushAccess: the reaper-side flush rewrites session.json
// only when lastAccess moved.
func TestHLSPersistFlushAccess(t *testing.T) {
	work := t.TempDir()
	m := newTestPersistManager(t, work, HLSConfig{}, nil)
	m.probe = fakeProbe
	if _, err := m.StartHLS("sess-f", testPersistURL, types.HLSSessionOptions{}); err != nil {
		t.Fatal(err)
	}
	jsonPath := filepath.Join(m.base, "sess-f", sessionFileName)

	// Nothing changed: no rewrite (remove the file to detect one).
	if err := os.Remove(jsonPath); err != nil {
		t.Fatal(err)
	}
	m.flushAccess()
	assertGone(t, jsonPath)

	later := time.Now().Add(time.Minute)
	m.mu.Lock()
	m.sessions["sess-f"].lastAccess.Store(later.UnixNano())
	m.mu.Unlock()
	m.flushAccess()
	rec, err := readSessionFile(filepath.Dir(jsonPath))
	if err != nil {
		t.Fatalf("session.json not rewritten after access: %v", err)
	}
	if !rec.LastAccess.Equal(later) {
		t.Errorf("persisted lastAccess = %v, want %v", rec.LastAccess, later)
	}
}

// TestHLSPersistSkipsStaleSession: persistSession never writes for a session
// that is no longer the one registered under its id.
func TestHLSPersistSkipsStaleSession(t *testing.T) {
	m := newTestPersistManager(t, t.TempDir(), HLSConfig{}, nil)
	dir := seedPersistedSession(t, m, "sess", time.Now())
	jsonPath := filepath.Join(dir, sessionFileName)
	before, _ := os.ReadFile(jsonPath)

	stale := &hlsSession{mediaURL: "http://93.184.216.35/other.mkv", dir: dir, duration: 99, tc: m.effectiveSessionConfig(sessionOverrides{})}
	stale.lastAccess.Store(time.Now().UnixNano())
	m.persistSession("sess", stale)
	if after, _ := os.ReadFile(jsonPath); !bytes.Equal(before, after) {
		t.Errorf("stale session overwrote session.json:\n%s", after)
	}
}

func TestHLSPersistExpiredRemovedOnLoad(t *testing.T) {
	work := t.TempDir()
	cfg := HLSConfig{SessionTTL: time.Hour}
	m1 := newTestPersistManager(t, work, cfg, nil)
	stale := seedPersistedSession(t, m1, "stale", time.Now().Add(-2*time.Hour))
	fresh := seedPersistedSession(t, m1, "fresh", time.Now().Add(-time.Minute))
	m1.CloseHLS()

	m2 := newTestPersistManager(t, work, cfg, nil)
	assertGone(t, stale)
	assertExists(t, fresh)
	if n := m2.Sessions(); n != 1 {
		t.Errorf("Sessions() = %d, want 1", n)
	}
}

func TestHLSPersistTmpFilesCleaned(t *testing.T) {
	work := t.TempDir()
	m1 := newTestPersistManager(t, work, HLSConfig{}, nil)
	dir := seedPersistedSession(t, m1, "tmpy", time.Now())
	m1.CloseHLS()
	files := map[string]bool{ // name -> should survive
		"seg0.ts":             true,
		"a1seg0.ts":           true,
		"sub0.vtt":            true,
		"seg1.ts.tmp.ts":      false,
		"a1seg1.ts.tmp.ts":    false,
		"sub1.vtt.tmp":        false,
		"session.json.tmp123": false,
	}
	for name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	newTestPersistManager(t, work, HLSConfig{}, nil)
	for name, keep := range files {
		if keep {
			assertExists(t, filepath.Join(dir, name))
		} else {
			assertGone(t, filepath.Join(dir, name))
		}
	}
	assertExists(t, filepath.Join(dir, sessionFileName))
}

// TestHLSPersistCorruptSessionRemoved: every session.json that fails
// validation deletes its session dir. Records built from a mutator are
// otherwise valid (correct id, fingerprint consistent with their config),
// so each case fails only for its own reason.
func TestHLSPersistCorruptSessionRemoved(t *testing.T) {
	work := t.TempDir()
	m1 := newTestPersistManager(t, work, HLSConfig{}, nil)
	good := seedPersistedSession(t, m1, "good", time.Now())
	defaultCfg := configToPersisted(m1.effectiveSessionConfig(sessionOverrides{}))
	m1.CloseHLS()

	record := func(id string, mut func(*persistedSession)) []byte {
		rec := persistedSession{
			Version:    sessionFormatVersion,
			ID:         id,
			MediaURL:   testPersistURL,
			Config:     defaultCfg,
			Probe:      persistedProbe{Duration: 20},
			LastAccess: time.Now(),
		}
		if mut != nil {
			mut(&rec)
		}
		if rec.Fingerprint == "" {
			tc := sessionConfig{
				videoBitrate: rec.Config.VideoBitrate, videoMaxrate: rec.Config.VideoMaxrate,
				videoBufsize: rec.Config.VideoBufsize, maxWidth: rec.Config.MaxWidth,
				maxHeight: rec.Config.MaxHeight, x264Preset: rec.Config.X264Preset,
				nvencPreset: rec.Config.NVENCPreset, qsvPreset: rec.Config.QSVPreset,
			}
			rec.Fingerprint = m1.outputFingerprint(tc, m1.segPreRollPlan(rec.Probe.IsTS, rec.Probe.SeekIndexed))
		}
		b, _ := json.Marshal(rec)
		return b
	}
	withField := func(id string) []byte {
		var obj map[string]any
		_ = json.Unmarshal(record(id, nil), &obj)
		obj["bogus"] = true
		b, _ := json.Marshal(obj)
		return b
	}
	cases := []struct {
		name string
		body []byte // nil = no session.json at all
	}{
		{"missing", nil},
		{"garbage", []byte("{not json")},
		{"trailing-object", append(record("trailing-object", nil), "{}"...)},
		{"trailing-brace", append(record("trailing-brace", nil), '}')},
		{"trailing-bracket", append(record("trailing-bracket", nil), ']')},
		{"unknown-field", withField("unknown-field")},
		{"wrong-version", record("wrong-version", func(r *persistedSession) { r.Version = 99 })},
		{"id-mismatch", record("id-mismatch", func(r *persistedSession) { r.ID = "someone-else" })},
		{"zero-duration", record("zero-duration", func(r *persistedSession) { r.Probe.Duration = 0 })},
		{"huge-duration", record("huge-duration", func(r *persistedSession) { r.Probe.Duration = 1e12 })},
		{"file-url", record("file-url", func(r *persistedSession) { r.MediaURL = "file:///etc/passwd" })},
		{"private-url", record("private-url", func(r *persistedSession) { r.MediaURL = "http://127.0.0.1:8080/x" })},
		{"metadata-url", record("metadata-url", func(r *persistedSession) { r.MediaURL = "http://169.254.169.254/latest" })},
		{"bad-bitrate", record("bad-bitrate", func(r *persistedSession) { r.Config.VideoBitrate = "8M -f null" })},
		{"bad-bufsize", record("bad-bufsize", func(r *persistedSession) { r.Config.VideoBufsize = "" })},
		{"bad-x264-preset", record("bad-x264-preset", func(r *persistedSession) { r.Config.X264Preset = "turbo" })},
		{"bad-nvenc-preset", record("bad-nvenc-preset", func(r *persistedSession) { r.Config.NVENCPreset = "p9" })},
		{"negative-width", record("negative-width", func(r *persistedSession) { r.Config.MaxWidth = -1 })},
	}
	dirs := map[string]string{}
	for _, tc := range cases {
		dir := filepath.Join(work, persistDirName, tc.name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if tc.body != nil {
			if err := os.WriteFile(filepath.Join(dir, sessionFileName), tc.body, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "seg0.ts"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		dirs[tc.name] = dir
	}
	// Sanity check the builder: an unmutated record must load.
	control := filepath.Join(work, persistDirName, "control")
	if err := os.MkdirAll(control, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(control, sessionFileName), record("control", nil), 0o600); err != nil {
		t.Fatal(err)
	}

	m2 := newTestPersistManager(t, work, HLSConfig{}, nil)
	for name, dir := range dirs {
		t.Run(name, func(t *testing.T) { assertGone(t, dir) })
	}
	assertExists(t, good)
	assertExists(t, control)
	if n := m2.Sessions(); n != 2 {
		t.Errorf("Sessions() = %d, want 2 (good + control)", n)
	}
}

// TestHLSPersistFingerprintMismatchRemoved: only process-wide knobs that
// shape segment output invalidate a session; per-session settings (env or
// /settings) and the hardware toggle don't, because the session keeps
// encoding with its own snapshot (see outputFingerprint).
func TestHLSPersistFingerprintMismatchRemoved(t *testing.T) {
	cases := []struct {
		name     string
		cfg      HLSConfig
		settings SettingsSource
		keep     bool
	}{
		{"unchanged", HLSConfig{}, nil, true},
		{"hw-accel-off", HLSConfig{}, stubSettings{"transcodeHardwareAccel": false}, true},
		{"settings-bitrate", HLSConfig{}, stubSettings{"transcodeMaxBitRate": float64(3_000_000)}, true},
		{"settings-profile", HLSConfig{}, stubSettings{"transcodeProfile": "slow"}, true},
		{"env-bitrate", HLSConfig{VideoBitrate: "4M"}, nil, true},
		{"env-max-height", HLSConfig{MaxHeight: 720}, nil, true},
		{"env-crf", HLSConfig{X264CRF: 18}, nil, false},
		{"env-vaapi-qp", HLSConfig{VAAPIQP: 30}, nil, false},
		{"env-audio-bitrate", HLSConfig{AudioBitrate: "128k"}, nil, false},
		{"env-audio-channels", HLSConfig{AudioChannels: 6}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			work := t.TempDir()
			m1 := newTestPersistManager(t, work, HLSConfig{}, nil)
			dir := seedPersistedSession(t, m1, "sess", time.Now())
			m1.CloseHLS()
			m2 := newTestPersistManager(t, work, tc.cfg, tc.settings)
			if tc.keep {
				assertExists(t, dir)
				if m2.Sessions() != 1 {
					t.Errorf("session not restored")
				}
			} else {
				assertGone(t, dir)
				if m2.Sessions() != 0 {
					t.Errorf("mismatched session restored")
				}
			}
		})
	}
}

// TestHLSPersistSeekPreroll: a changed STREMIO_HLS_SEEK_PREROLL discards
// restored sessions in an indexed container (their segments start decoding
// from different frames) and keeps every other session; an unchanged one
// keeps them all, and the container class survives the round trip.
func TestHLSPersistSeekPreroll(t *testing.T) {
	indexed := func(s *hlsSession) { s.seekIndexed = true }
	ts := func(s *hlsSession) { s.isTS = true }
	cases := []struct {
		name         string
		was, now     time.Duration
		keepIndexed  bool
		keepTSOrElse bool
	}{
		{"unchanged default", 0, 0, true, true},
		{"unchanged 2s", 2 * time.Second, 2 * time.Second, true, true},
		{"default to 2s", 0, 2 * time.Second, false, true},
		{"2s to default", 2 * time.Second, 0, false, true},
		{"2s to none", 2 * time.Second, SeekPrerollNone, false, true},
		{"default to 10s", 0, 10 * time.Second, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			work := t.TempDir()
			m1 := newTestPersistManager(t, work, HLSConfig{SeekPreroll: tc.was}, nil)
			dirIdx := seedPersistedSessionWith(t, m1, "mkv", time.Now(), indexed)
			dirTS := seedPersistedSessionWith(t, m1, "ts", time.Now(), ts)
			dirOther := seedPersistedSession(t, m1, "other", time.Now())
			m1.CloseHLS()
			m2 := newTestPersistManager(t, work, HLSConfig{SeekPreroll: tc.now}, nil)
			check := func(dir string, keep bool) {
				t.Helper()
				if keep {
					assertExists(t, dir)
				} else {
					assertGone(t, dir)
				}
			}
			check(dirIdx, tc.keepIndexed)
			check(dirTS, tc.keepTSOrElse)
			check(dirOther, tc.keepTSOrElse)
			if tc.keepIndexed {
				m2.mu.Lock()
				s := m2.sessions["mkv"]
				m2.mu.Unlock()
				if s == nil || !s.seekIndexed || s.isTS {
					t.Errorf("restored indexed session lost its container class: %+v", s)
				}
			}
		})
	}
}

func TestHLSPersistMaxSessionsOnLoad(t *testing.T) {
	work := t.TempDir()
	m1 := newTestPersistManager(t, work, HLSConfig{}, nil)
	now := time.Now()
	oldest := seedPersistedSession(t, m1, "oldest", now.Add(-30*time.Second))
	middle := seedPersistedSession(t, m1, "middle", now.Add(-20*time.Second))
	newest := seedPersistedSession(t, m1, "newest", now.Add(-10*time.Second))
	m1.CloseHLS()

	logs := captureLogs(t)
	m2 := newTestPersistManager(t, work, HLSConfig{MaxSessions: 2}, nil)
	assertGone(t, oldest)
	assertExists(t, middle)
	assertExists(t, newest)
	if n := m2.Sessions(); n != 2 {
		t.Errorf("Sessions() = %d, want 2", n)
	}
	if !strings.Contains(logs.String(), "over STREMIO_HLS_MAX_SESSIONS") {
		t.Errorf("MaxSessions discard not logged:\n%s", logs.String())
	}
}

// TestHLSPersistCloseKeepsDir: CloseHLS stops the reaper, forgets the
// sessions and releases the lock but keeps the persist dir; eviction still
// deletes a session dir.
func TestHLSPersistCloseKeepsDir(t *testing.T) {
	work := t.TempDir()
	m := newTestPersistManager(t, work, HLSConfig{}, nil)
	evicted := seedPersistedSession(t, m, "evicted", time.Now())
	kept := seedPersistedSession(t, m, "kept", time.Now())
	m.CloseHLS()
	m2 := newTestPersistManager(t, work, HLSConfig{}, nil)

	m2.mu.Lock()
	m2.sessions["evicted"].lastAccess.Store(time.Now().Add(-2 * m2.cfg.SessionTTL).UnixNano())
	m2.mu.Unlock()
	m2.evictIdle()
	assertGone(t, evicted)

	m2.CloseHLS()
	m2.CloseHLS() // idempotent
	select {
	case <-m2.stopCh:
	default:
		t.Error("CloseHLS did not stop the reaper")
	}
	if m2.Sessions() != 0 {
		t.Error("CloseHLS left sessions in memory")
	}
	assertExists(t, m2.base)
	assertExists(t, filepath.Join(kept, sessionFileName))
	// The lock was released: a new manager can take the dir.
	newTestPersistManager(t, work, HLSConfig{}, nil)
}

// TestHLSPersistLockExclusive: a second manager (another process, in
// production) can't take a persist dir that is in use; it warns and falls
// back to the non-persistent layout, and the dir becomes available again
// once the first releases it.
func TestHLSPersistLockExclusive(t *testing.T) {
	if !persistLockSupported {
		t.Skip("no persist dir locking on this platform")
	}
	work := t.TempDir()
	cfg := HLSConfig{WorkDir: work, Persist: true}.normalize(1)
	base1, persist1, lock1 := newHLSBase(cfg)
	if !persist1 {
		t.Fatal("first manager did not get the persist dir")
	}

	logs := captureLogs(t)
	base2, persist2, lock2 := newHLSBase(cfg)
	defer os.RemoveAll(base2)
	if persist2 || lock2 != nil || base2 == base1 {
		t.Fatalf("second manager shared a locked persist dir (base=%q)", base2)
	}
	if filepath.Dir(base2) != work || !strings.HasPrefix(filepath.Base(base2), "stremio-hls-") {
		t.Errorf("fallback base = %q, want a random stremio-hls-* dir under %q", base2, work)
	}
	if !strings.Contains(logs.String(), errPersistLocked.Error()) {
		t.Errorf("lock conflict not logged:\n%s", logs.String())
	}

	if err := lock1.Close(); err != nil {
		t.Fatal(err)
	}
	_, persist3, lock3 := newHLSBase(cfg)
	if !persist3 {
		t.Fatal("persist dir still locked after release")
	}
	_ = lock3.Close()
}

// TestHLSPersistIgnoresInvalidEntries: directory names StartHLS would never
// accept, and stray files, are neither loaded nor deleted.
func TestHLSPersistIgnoresInvalidEntries(t *testing.T) {
	work := t.TempDir()
	m1 := newTestPersistManager(t, work, HLSConfig{}, nil)
	good := seedPersistedSession(t, m1, "good", time.Now())
	m1.CloseHLS()
	// Craft a perfectly valid session.json under a traversal-looking name.
	bad := filepath.Join(m1.base, "evil..name")
	if err := os.Mkdir(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(good, sessionFileName))
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(`"id": "good"`), []byte(`"id": "evil..name"`), 1)
	if err := os.WriteFile(filepath.Join(bad, sessionFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(m1.base, "stray.txt")
	if err := os.WriteFile(stray, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	m2 := newTestPersistManager(t, work, HLSConfig{}, nil)
	if n := m2.Sessions(); n != 1 {
		t.Errorf("Sessions() = %d, want 1", n)
	}
	m2.mu.Lock()
	_, loaded := m2.sessions["evil..name"]
	m2.mu.Unlock()
	if loaded {
		t.Error("session with a traversal-looking id was loaded")
	}
	assertExists(t, bad)
	assertExists(t, stray)
}

// TestHLSPersistSymlinkedSessionIgnored: a symlink in the persist dir, even
// one pointing at a valid session, is neither followed, loaded nor deleted.
func TestHLSPersistSymlinkedSessionIgnored(t *testing.T) {
	work := t.TempDir()
	m1 := newTestPersistManager(t, work, HLSConfig{}, nil)
	realDir := seedPersistedSession(t, m1, "real", time.Now())
	m1.CloseHLS()
	// Move the valid session outside the persist dir and link it back in
	// under its own id.
	outside := filepath.Join(t.TempDir(), "real")
	if err := os.Rename(realDir, outside); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, outside, realDir)

	m2 := newTestPersistManager(t, work, HLSConfig{}, nil)
	if n := m2.Sessions(); n != 0 {
		t.Errorf("Sessions() = %d, want 0 (symlink must not be loaded)", n)
	}
	assertExists(t, realDir)
	assertExists(t, filepath.Join(outside, sessionFileName))
}

// TestHLSPersistRefusesSymlinkedDir: a persist dir that is a symlink (e.g.
// planted by another user pointing somewhere they control) is refused.
func TestHLSPersistRefusesSymlinkedDir(t *testing.T) {
	work := t.TempDir()
	target := t.TempDir()
	symlinkOrSkip(t, target, filepath.Join(work, persistDirName))
	logs := captureLogs(t)
	base, persist, _ := newHLSBase(HLSConfig{WorkDir: work, Persist: true})
	defer os.RemoveAll(base)
	if persist {
		t.Fatal("symlinked persist dir accepted")
	}
	if !strings.HasPrefix(filepath.Base(base), "stremio-hls-") || filepath.Base(base) == persistDirName {
		t.Errorf("fallback base = %q, want a random stremio-hls-* dir", base)
	}
	if !strings.Contains(logs.String(), "HLS persist dir unusable") {
		t.Errorf("refusal not logged:\n%s", logs.String())
	}
}

// TestHLSPersistTightensDirMode: on POSIX a pre-existing persist dir that is
// ours but too open (group/world readable or writable) is chmod'ed to 0700
// and used. Windows has no POSIX mode bits to check.
func TestHLSPersistTightensDirMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	for _, mode := range []os.FileMode{0o755, 0o777} {
		work := t.TempDir()
		dir := filepath.Join(work, persistDirName)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, mode); err != nil { // bypass umask
			t.Fatal(err)
		}
		base, persist, lock := newHLSBase(HLSConfig{WorkDir: work, Persist: true})
		if !persist || base != dir {
			t.Fatalf("mode %04o: our own persist dir refused (base=%q)", mode, base)
		}
		_ = lock.Close()
		assertMode(t, dir, 0o700)
	}
}

// TestHLSPersistConcurrentWithReaper exercises the persistence paths that
// run concurrently in production (StartHLS's creation write, HLSFile
// touching lastAccess, the reaper's periodic flush, CloseHLS's final flush)
// with a fast-ticking reaper, for the race detector. It then checks the
// final session.json carries each session's last in-memory lastAccess.
func TestHLSPersistConcurrentWithReaper(t *testing.T) {
	work := t.TempDir()
	cfg := HLSConfig{SessionTTL: time.Hour, ReaperInterval: time.Millisecond}
	m := newTestPersistManager(t, work, cfg, nil)
	m.probe = fakeProbe
	m.startReaper()

	ids := []string{"c0", "c1", "c2", "c3"}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if _, err := m.StartHLS(id, testPersistURL, types.HLSSessionOptions{}); err != nil {
					t.Errorf("StartHLS(%s): %v", id, err)
					return
				}
				if _, _, err := m.HLSFile(context.Background(), id, "playlist.m3u8"); err != nil {
					t.Errorf("HLSFile(%s): %v", id, err)
					return
				}
			}
		}(id)
	}
	wg.Wait()

	want := map[string]int64{}
	m.mu.Lock()
	for id, s := range m.sessions {
		want[id] = s.lastAccess.Load()
	}
	m.mu.Unlock()
	m.CloseHLS()

	for _, id := range ids {
		rec, err := readSessionFile(filepath.Join(m.base, id))
		if err != nil {
			t.Errorf("%s: %v", id, err)
			continue
		}
		if got := rec.LastAccess.UnixNano(); got != want[id] {
			t.Errorf("%s: persisted lastAccess %d, want final in-memory %d", id, got, want[id])
		}
	}
	if n := newTestPersistManager(t, work, cfg, nil).Sessions(); n != len(ids) {
		t.Errorf("restored %d sessions, want %d", n, len(ids))
	}
}

// TestHLSPersistRestoreHonoursSessionTTL: on load a session is judged by the
// idle TTL the reaper would apply to it, i.e. its own ttl override when it
// has one, not the global SessionTTL.
func TestHLSPersistRestoreHonoursSessionTTL(t *testing.T) {
	work := t.TempDir()
	cfg := HLSConfig{SessionTTL: time.Hour}
	m1 := newTestPersistManager(t, work, cfg, nil)
	idle := time.Now().Add(-2 * time.Hour)
	long := seedPersistedSessionWith(t, m1, "long", idle, func(s *hlsSession) { s.ttl.Store(int64(3 * time.Hour)) })
	short := seedPersistedSessionWith(t, m1, "short", idle, func(s *hlsSession) { s.ttl.Store(int64(90 * time.Minute)) })
	plain := seedPersistedSession(t, m1, "plain", idle)
	bad := seedPersistedSessionWith(t, m1, "bad", time.Now(), func(s *hlsSession) { s.ttl.Store(int64(time.Second)) })
	m1.CloseHLS()

	// A ttl shorter than the global TTL wins too: it is not max(ttl, global).
	work2 := t.TempDir()
	m3 := newTestPersistManager(t, work2, HLSConfig{SessionTTL: 3 * time.Hour}, nil)
	shorter := seedPersistedSessionWith(t, m3, "shorter", idle, func(s *hlsSession) { s.ttl.Store(int64(90 * time.Minute)) })
	m3.CloseHLS()
	newTestPersistManager(t, work2, HLSConfig{SessionTTL: 3 * time.Hour}, nil)
	assertGone(t, shorter)

	m2 := newTestPersistManager(t, work, cfg, nil)
	assertExists(t, long)
	assertGone(t, short)
	assertGone(t, plain)
	assertGone(t, bad) // below the 60s floor an override accepts
	m2.mu.Lock()
	got := m2.sessions["long"]
	m2.mu.Unlock()
	if got == nil {
		t.Fatal("session with a 3h ttl override was not restored")
	}
	if ttl := time.Duration(got.ttl.Load()); ttl != 3*time.Hour {
		t.Errorf("restored ttl = %s, want 3h", ttl)
	}
}

// TestHLSPersistIdleEvictionDisabled: with STREMIO_HLS_SESSION_TTL=0
// (DisableIdleEviction) persisted sessions are not discarded on load for
// being idle, unless they carry their own ttl, and the default-TTL warning
// (normalize turns the 0 into the 60s default) is not logged.
func TestHLSPersistIdleEvictionDisabled(t *testing.T) {
	work := t.TempDir()
	logs := captureLogs(t)
	cfg := HLSConfig{DisableIdleEviction: true}
	m1 := newTestPersistManager(t, work, cfg, nil)
	idle := time.Now().Add(-24 * time.Hour)
	old := seedPersistedSession(t, m1, "old", idle)
	own := seedPersistedSessionWith(t, m1, "own", idle, func(s *hlsSession) { s.ttl.Store(int64(time.Hour)) })
	m1.CloseHLS()

	m2 := newTestPersistManager(t, work, cfg, nil)
	assertExists(t, old)
	assertGone(t, own) // a per-session ttl still applies, as in evictIdle
	if n := m2.Sessions(); n != 1 {
		t.Errorf("Sessions() = %d, want 1", n)
	}
	if strings.Contains(logs.String(), "STREMIO_HLS_SESSION_TTL is still the default") {
		t.Errorf("default-TTL warning logged with idle eviction disabled; logs:\n%s", logs)
	}
}
