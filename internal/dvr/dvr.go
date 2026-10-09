// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

// Package dvr records live streams to disk with ffmpeg (stream copy, MPEG-TS),
// mirroring the DVR of EasyProxy (github.com/realbestia1/EasyProxy,
// services/recording_manager.py): a recording is started in the background,
// listed with the JSON shape DVR addons consume, can be stopped (gracefully, so
// the file stays playable), watched while it grows, and deleted.
//
// The Manager knows nothing about HTTP or about how a source URL is resolved:
// callers hand it an InputURL that ffmpeg can read as-is (the server's own
// /proxy/... endpoint on loopback), which is what makes headers, embed
// extraction and the upstream proxy apply to recordings.
package dvr

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/M0Rf30/stremio-server-go/internal/logging"
)

// Recording states (EasyProxy's status vocabulary; DVR addons switch on them).
const (
	StatusStarting  = "starting"
	StatusRecording = "recording"
	StatusCompleted = "completed" // ffmpeg ended by itself or the duration cap was reached
	StatusStopped   = "stopped"   // stopped by the user / server shutdown
	StatusFailed    = "failed"
)

// Errors returned by Manager operations.
var (
	ErrNoFFmpeg  = errors.New("ffmpeg not found in PATH")
	ErrBusy      = errors.New("too many active recordings")
	ErrDiskFull  = errors.New("recordings storage quota reached")
	ErrDuplicate = errors.New("already recording this source")
	ErrNotFound  = errors.New("recording not found")
	ErrNotActive = errors.New("recording is not active")
	ErrClosed    = errors.New("dvr manager closed")
)

const (
	defaultMaxActive   = 2
	defaultStopGrace   = 10 * time.Second
	defaultKillGrace   = 5 * time.Second
	defaultOverrun     = 30 * time.Second
	defaultDuration    = 4 * time.Hour
	defaultMaxDuration = 8 * time.Hour
	maxNameRunes       = 100
	maxFileNameRunes   = 50
	stderrTail         = 4096
	sidecarSuffix      = ".json"
	mediaSuffix        = ".ts"
)

// idPattern matches the ids this package generates: YYYYMMDD_HHMMSS_<8 hex>.
var idPattern = regexp.MustCompile(`^[0-9]{8}_[0-9]{6}_[0-9a-f]{8}$`)

// ValidID reports whether id has the shape of a recording id. Anything else
// (including path separators and ".." ) can never name a file.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// Proc is a running recorder process.
type Proc interface {
	// Wait blocks until the process exits.
	Wait() error
	// Quit asks the recorder to finish cleanly (ffmpeg: 'q' on stdin).
	Quit()
	// Kill terminates the recorder (SIGINT/SIGTERM-like first, hard kill after).
	Kill()
	// Output returns the tail of the recorder's diagnostic output.
	Output() string
}

// StartFunc launches a recorder. It is the injection seam for tests.
type StartFunc func(bin string, args []string) (Proc, error)

// Config configures a Manager. Zero values pick the documented defaults.
type Config struct {
	Dir             string        // recordings directory (created)
	FFmpeg          string        // ffmpeg binary; "" = "ffmpeg"
	DefaultDuration time.Duration // used when a request carries no duration
	MaxDuration     time.Duration // hard per-recording cap
	MaxActive       int           // concurrent recorders; <=0 = 2
	MaxBytes        int64         // total recordings size cap; 0 = unlimited
	Retention       time.Duration // age out finished recordings; 0 = keep
	StopGrace       time.Duration // wait after 'q' before killing; 0 = 10s
	Overrun         time.Duration // wall-clock slack past the duration cap; 0 = 30s
	SweepEvery      time.Duration // janitor period; 0 = 1h

	Start    StartFunc                    // nil = exec ffmpeg
	LookPath func(string) (string, error) // nil = exec.LookPath
	Now      func() time.Time             // nil = time.Now
	Log      *slog.Logger                 // nil = logging.For("dvr")
	Redact   func(rawurl string) string   // nil = RedactURL
}

// Recording is the JSON shape served by /api/recordings (EasyProxy-compatible,
// fields addons read: id, name, url, file_path, status,
// started_at, stopped_at, duration_seconds, file_size_bytes, is_active,
// elapsed_seconds).
type Recording struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	URL             string  `json:"url"`
	FilePath        string  `json:"file_path"` // base name inside the recordings dir
	Status          string  `json:"status"`
	StartedAt       string  `json:"started_at"`
	StoppedAt       *string `json:"stopped_at"`
	DurationSeconds *int64  `json:"duration_seconds"`
	FileSizeBytes   *int64  `json:"file_size_bytes"`
	ErrorMessage    *string `json:"error_message"`
	IsActive        bool    `json:"is_active"`
	ElapsedSeconds  *int64  `json:"elapsed_seconds,omitempty"`
	MaxDurationSecs int64   `json:"max_duration_seconds"`
}

// stored is the on-disk sidecar (<id>.json).
type stored struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	URL         string    `json:"url"`
	File        string    `json:"file"`
	Status      string    `json:"status"`
	StartedAt   time.Time `json:"started_at"`
	StoppedAt   time.Time `json:"stopped_at,omitempty"`
	Seconds     int64     `json:"duration_seconds,omitempty"`
	Size        int64     `json:"file_size_bytes,omitempty"`
	Err         string    `json:"error_message,omitempty"`
	MaxDuration int64     `json:"max_duration_seconds"`
}

type entry struct {
	stored
	proc     Proc
	stopCh   chan struct{} // closed to request a stop
	stopOnce sync.Once
	finished chan struct{} // closed once the supervisor recorded the final state
	stopReq  bool          // a stop (user/shutdown) was requested
}

func (e *entry) active() bool {
	return e.Status == StatusStarting || e.Status == StatusRecording
}

// Manager owns the recordings directory and the recorder processes.
type Manager struct {
	cfg Config
	log *slog.Logger

	mu     sync.Mutex
	recs   map[string]*entry
	closed bool

	wg      sync.WaitGroup
	stopJan chan struct{}
	janOnce sync.Once
	closeMu sync.Once
}

// New creates the recordings directory, loads existing recordings, repairs
// the ones a previous process left half-written and starts the retention
// janitor.
func New(cfg Config) (*Manager, error) {
	if cfg.Dir == "" {
		return nil, errors.New("dvr: empty recordings directory")
	}
	dir, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return nil, err
	}
	cfg.Dir = dir
	if cfg.FFmpeg == "" {
		cfg.FFmpeg = "ffmpeg"
	}
	if cfg.MaxDuration <= 0 {
		cfg.MaxDuration = defaultMaxDuration
	}
	if cfg.DefaultDuration <= 0 {
		cfg.DefaultDuration = defaultDuration
	}
	if cfg.DefaultDuration > cfg.MaxDuration {
		cfg.DefaultDuration = cfg.MaxDuration
	}
	if cfg.MaxActive <= 0 {
		cfg.MaxActive = defaultMaxActive
	}
	if cfg.StopGrace <= 0 {
		cfg.StopGrace = defaultStopGrace
	}
	if cfg.Overrun <= 0 {
		cfg.Overrun = defaultOverrun
	}
	if cfg.SweepEvery <= 0 {
		cfg.SweepEvery = time.Hour
	}
	if cfg.Start == nil {
		cfg.Start = execStart
	}
	if cfg.LookPath == nil {
		cfg.LookPath = exec.LookPath
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Redact == nil {
		cfg.Redact = RedactURL
	}
	if err := os.MkdirAll(cfg.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("dvr: create %s: %w", cfg.Dir, err)
	}
	m := &Manager{cfg: cfg, recs: map[string]*entry{}, stopJan: make(chan struct{})}
	m.log = cfg.Log
	if m.log == nil {
		m.log = logging.For("dvr")
	}
	m.load()
	m.sweep()
	if cfg.Retention > 0 {
		m.wg.Add(1)
		go m.janitor()
	}
	return m, nil
}

// Dir returns the (absolute) recordings directory.
func (m *Manager) Dir() string { return m.cfg.Dir }

// MaxActive returns the effective concurrent-recording cap.
func (m *Manager) MaxActive() int { return m.cfg.MaxActive }

// MaxDuration returns the effective per-recording cap.
func (m *Manager) MaxDuration() time.Duration { return m.cfg.MaxDuration }

// ClampDuration applies the default (d <= 0) and the cap (d > MaxDuration).
func (m *Manager) ClampDuration(d time.Duration) time.Duration {
	if d <= 0 {
		return m.cfg.DefaultDuration
	}
	if d > m.cfg.MaxDuration {
		return m.cfg.MaxDuration
	}
	return d
}

// FFmpegAvailable reports whether the recorder binary can be found.
func (m *Manager) FFmpegAvailable() bool {
	_, err := m.cfg.LookPath(m.cfg.FFmpeg)
	return err == nil
}

// ---- naming ---------------------------------------------------------------

// SanitizeName returns the display name (control characters stripped, length
// capped) and the file-name stem (letters, digits, space, '-' and '_' only;
// spaces become '_'; max 50 runes) for a client-supplied title. The stem can
// never contain a path separator or a dot, so it cannot traverse out of the
// recordings directory.
func SanitizeName(name string) (display, stem string) {
	var d strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(name) {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			continue
		}
		if n >= maxNameRunes {
			break
		}
		d.WriteRune(r)
		n++
	}
	display = strings.TrimSpace(d.String())
	var s strings.Builder
	n = 0
	for _, r := range display {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			s.WriteRune(r)
		case r == ' ':
			s.WriteRune('_')
		default:
			continue
		}
		n++
		if n >= maxFileNameRunes {
			break
		}
	}
	stem = strings.Trim(s.String(), "_")
	if stem == "" {
		stem = "recording"
	}
	return display, stem
}

// RedactURL strips credentials this server issues (api_password, token) from
// a source URL before it is stored or listed. The query order and the rest of
// the URL are preserved byte for byte.
func RedactURL(raw string) string {
	i := strings.IndexByte(raw, '?')
	if i < 0 {
		return raw
	}
	head, rest := raw[:i], raw[i+1:]
	frag := ""
	if j := strings.IndexByte(rest, '#'); j >= 0 {
		rest, frag = rest[:j], rest[j:]
	}
	var keep []string
	for _, kv := range strings.Split(rest, "&") {
		k, _, _ := strings.Cut(kv, "=")
		if dk, err := url.QueryUnescape(k); err == nil {
			k = dk
		}
		if k == "api_password" || k == "token" {
			continue
		}
		keep = append(keep, kv)
	}
	if len(keep) == 0 {
		return head + frag
	}
	return head + "?" + strings.Join(keep, "&") + frag
}

func newID(now time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return now.UTC().Format("20060102_150405") + "_" + hex.EncodeToString(b[:])
}

// ---- start ----------------------------------------------------------------

// StartRequest describes one recording.
type StartRequest struct {
	Name     string        // client-supplied title (sanitised)
	Source   string        // the URL the client asked to record (stored, redacted)
	Input    string        // what ffmpeg reads (never stored or listed)
	Duration time.Duration // 0 = default; always clamped to the cap
}

// Start launches a background recording. When the same source is already
// being recorded it returns that recording together with ErrDuplicate.
func (m *Manager) Start(req StartRequest) (Recording, error) {
	source := m.cfg.Redact(req.Source)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Recording{}, ErrClosed
	}
	for _, e := range m.recs {
		if e.active() && e.URL == source {
			return m.viewLocked(e), ErrDuplicate
		}
	}
	bin, err := m.cfg.LookPath(m.cfg.FFmpeg)
	if err != nil {
		return Recording{}, ErrNoFFmpeg
	}
	active := 0
	for _, e := range m.recs {
		if e.active() {
			active++
		}
	}
	if active >= m.cfg.MaxActive {
		return Recording{}, ErrBusy
	}
	var maxBytes int64
	if m.cfg.MaxBytes > 0 {
		used := m.usedBytesLocked()
		if used >= m.cfg.MaxBytes {
			return Recording{}, ErrDiskFull
		}
		maxBytes = m.cfg.MaxBytes - used
	}

	now := m.cfg.Now().UTC()
	display, stem := SanitizeName(req.Name)
	if display == "" {
		display = "Recording " + now.Format("2006-01-02 15:04")
	}
	id := newID(now)
	file := id + "_" + stem + mediaSuffix
	dur := m.ClampDuration(req.Duration)
	e := &entry{
		stored: stored{
			ID: id, Name: display, URL: source, File: file,
			Status: StatusStarting, StartedAt: now, MaxDuration: int64(dur / time.Second),
		},
		stopCh:   make(chan struct{}),
		finished: make(chan struct{}),
	}
	out := m.pathOf(e)
	if out == "" {
		return Recording{}, errors.New("dvr: invalid output path")
	}
	if err := m.writeSidecar(&e.stored); err != nil {
		return Recording{}, fmt.Errorf("dvr: persist: %w", err)
	}
	proc, err := m.cfg.Start(bin, BuildArgs(req.Input, out, dur, maxBytes))
	if err != nil {
		m.removeFiles(&e.stored)
		return Recording{}, fmt.Errorf("dvr: start ffmpeg: %w", err)
	}
	e.proc = proc
	e.Status = StatusRecording
	_ = m.writeSidecar(&e.stored)
	m.recs[id] = e
	m.wg.Add(1)
	go m.supervise(e, dur)
	m.log.Info("recording started", "id", id, "name", display, "max_s", int64(dur/time.Second))
	return m.viewLocked(e), nil
}

// BuildArgs returns the ffmpeg argument vector: stream copy of the first video
// and audio stream into MPEG-TS (growing-file friendly, like EasyProxy), bounded
// by dur and, when maxBytes > 0, by the remaining storage quota. The protocol
// whitelist confines ffmpeg to HTTP(S): the input is always this server's own
// proxy endpoint, which has already rewritten every playlist URI.
func BuildArgs(input, out string, dur time.Duration, maxBytes int64) []string {
	args := []string{
		"-hide_banner", "-loglevel", "warning", "-nostats", "-y",
		"-protocol_whitelist", "http,https,tcp,tls,crypto",
		"-fflags", "+genpts+discardcorrupt+igndts",
		"-rw_timeout", "30000000",
		"-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_delay_max", "2",
		"-i", input,
		"-t", strconv.FormatInt(int64(dur/time.Second), 10),
		"-map", "0:v:0?", "-map", "0:a:0?",
		"-c", "copy",
	}
	if maxBytes > 0 {
		args = append(args, "-fs", strconv.FormatInt(maxBytes, 10))
	}
	return append(args, "-f", "mpegts", out)
}

// supervise waits for the recorder, enforces the wall-clock cap and records
// the final state. It always closes e.finished.
func (m *Manager) supervise(e *entry, dur time.Duration) {
	defer m.wg.Done()
	done := make(chan error, 1)
	go func() { done <- e.proc.Wait() }()
	timer := time.NewTimer(dur + m.cfg.Overrun)
	defer timer.Stop()

	var waitErr error
	capped := false
	select {
	case waitErr = <-done:
	case <-timer.C:
		capped = true
		waitErr = m.halt(e, done)
	case <-e.stopCh:
		waitErr = m.halt(e, done)
	}

	m.mu.Lock()
	now := m.cfg.Now().UTC()
	e.StoppedAt = now
	e.Seconds = int64(now.Sub(e.StartedAt) / time.Second)
	if st, err := os.Stat(m.pathOf(e)); err == nil {
		e.Size = st.Size()
	}
	switch {
	case e.stopReq:
		e.Status = StatusStopped
	case capped || waitErr == nil:
		e.Status = StatusCompleted
	default:
		e.Status = StatusFailed
		e.Err = failureMessage(waitErr, e.proc.Output())
	}
	if e.Size == 0 && e.Status != StatusStopped {
		// Nothing was captured: drop the empty partial file, keep the row so the
		// failure stays visible until retention/delete.
		_ = os.Remove(m.pathOf(e))
	}
	_ = m.writeSidecar(&e.stored)
	status, size := e.Status, e.Size
	close(e.finished)
	m.mu.Unlock()
	m.log.Info("recording finished", "id", e.ID, "status", status, "bytes", size)
}

// halt stops the recorder: 'q' (clean trailer), then kill after StopGrace.
func (m *Manager) halt(e *entry, done <-chan error) error {
	e.proc.Quit()
	select {
	case err := <-done:
		return err
	case <-time.After(m.cfg.StopGrace):
	}
	e.proc.Kill()
	select {
	case err := <-done:
		return err
	case <-time.After(defaultKillGrace):
		return errors.New("recorder did not exit")
	}
}

// secretParam matches credentials that may appear in ffmpeg's diagnostics
// (the loopback input URL carries api_password and h_* header overrides).
var secretParam = regexp.MustCompile(`(?i)\b(api_password|token|h_[a-z0-9_%.-]+)=[^&\s"']+`)

func failureMessage(err error, out string) string {
	out = strings.TrimSpace(out)
	if len(out) > 500 {
		out = out[len(out)-500:]
	}
	out = secretParam.ReplaceAllString(out, "$1=***")
	if out == "" {
		return err.Error()
	}
	return err.Error() + ": " + out
}

// Stop gracefully stops an active recording and returns its final state.
func (m *Manager) Stop(id string) (Recording, error) {
	m.mu.Lock()
	e, ok := m.recs[id]
	if !ok {
		m.mu.Unlock()
		return Recording{}, ErrNotFound
	}
	if !e.active() {
		v := m.viewLocked(e)
		m.mu.Unlock()
		return v, ErrNotActive
	}
	e.stopReq = true
	e.stopOnce.Do(func() { close(e.stopCh) })
	m.mu.Unlock()
	<-e.finished
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.viewLocked(e), nil
}

// Delete stops the recording if needed and removes its file and metadata.
func (m *Manager) Delete(id string) error {
	if _, err := m.Stop(id); err != nil && !errors.Is(err, ErrNotActive) {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.recs[id]
	if !ok {
		return ErrNotFound
	}
	m.removeFiles(&e.stored)
	delete(m.recs, id)
	return nil
}

// DeleteAll deletes every recording (all statuses when status == "") and
// returns how many were removed.
func (m *Manager) DeleteAll(status string) int {
	n := 0
	for _, r := range m.List(status) {
		if m.Delete(r.ID) == nil {
			n++
		}
	}
	return n
}

// Close stops every active recording gracefully, halts the janitor and waits
// for all supervisors. Safe to call more than once.
func (m *Manager) Close() error {
	m.closeMu.Do(func() {
		m.mu.Lock()
		m.closed = true
		var live []*entry
		for _, e := range m.recs {
			if e.active() {
				e.stopReq = true
				e.stopOnce.Do(func() { close(e.stopCh) })
				live = append(live, e)
			}
		}
		m.mu.Unlock()
		m.janOnce.Do(func() { close(m.stopJan) })
		for _, e := range live {
			<-e.finished
		}
		m.wg.Wait()
	})
	return nil
}

// ---- queries --------------------------------------------------------------

// Get returns a snapshot of one recording.
func (m *Manager) Get(id string) (Recording, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.recs[id]
	if !ok {
		return Recording{}, false
	}
	return m.viewLocked(e), true
}

// List returns snapshots, newest first, optionally filtered by status.
func (m *Manager) List(status string) []Recording {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Recording, 0, len(m.recs))
	for _, e := range m.recs {
		if status != "" && e.Status != status {
			continue
		}
		out = append(out, m.viewLocked(e))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt != out[j].StartedAt {
			return out[i].StartedAt > out[j].StartedAt
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// Active returns the recordings that are currently running.
func (m *Manager) Active() []Recording {
	var out []Recording
	for _, r := range m.List("") {
		if r.IsActive {
			out = append(out, r)
		}
	}
	return out
}

// IsActive reports whether id is still being recorded.
func (m *Manager) IsActive(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.recs[id]
	return ok && e.active()
}

// UsedBytes returns the total size of all recording files.
func (m *Manager) UsedBytes() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.usedBytesLocked()
}

// Open opens the media file of a recording for reading.
func (m *Manager) Open(id string) (*os.File, Recording, error) {
	m.mu.Lock()
	e, ok := m.recs[id]
	if !ok {
		m.mu.Unlock()
		return nil, Recording{}, ErrNotFound
	}
	path := m.pathOf(e)
	v := m.viewLocked(e)
	m.mu.Unlock()
	if path == "" {
		return nil, v, ErrNotFound
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, v, ErrNotFound
	}
	return f, v, nil
}

func (m *Manager) viewLocked(e *entry) Recording {
	r := Recording{
		ID: e.ID, Name: e.Name, URL: e.URL, FilePath: e.File, Status: e.Status,
		StartedAt:       e.StartedAt.UTC().Format(time.RFC3339),
		IsActive:        e.active(),
		MaxDurationSecs: e.MaxDuration,
	}
	if e.active() {
		el := int64(m.cfg.Now().Sub(e.StartedAt) / time.Second)
		r.ElapsedSeconds = &el
		if st, err := os.Stat(m.pathOf(e)); err == nil {
			sz := st.Size()
			r.FileSizeBytes = &sz
		}
		return r
	}
	if !e.StoppedAt.IsZero() {
		s := e.StoppedAt.UTC().Format(time.RFC3339)
		r.StoppedAt = &s
		d := e.Seconds
		r.DurationSeconds = &d
	}
	sz := e.Size
	r.FileSizeBytes = &sz
	if e.Err != "" {
		s := e.Err
		r.ErrorMessage = &s
	}
	return r
}

func (m *Manager) usedBytesLocked() int64 {
	var n int64
	for _, e := range m.recs {
		if st, err := os.Stat(m.pathOf(e)); err == nil {
			n += st.Size()
		}
	}
	return n
}

// ---- files ----------------------------------------------------------------

// pathOf returns the absolute media path of e, or "" when its file name is not
// a plain base name inside the recordings directory.
func (m *Manager) pathOf(e *entry) string {
	f := e.File
	if f == "" || f != filepath.Base(f) || !strings.HasSuffix(f, mediaSuffix) ||
		strings.ContainsAny(f, `/\`) || !strings.HasPrefix(f, e.ID+"_") {
		return ""
	}
	return filepath.Join(m.cfg.Dir, f)
}

func (m *Manager) sidecarPath(id string) string {
	return filepath.Join(m.cfg.Dir, id+sidecarSuffix)
}

func (m *Manager) writeSidecar(s *stored) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.cfg.Dir, ".rec-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(name)
		return errors.Join(werr, cerr)
	}
	if err := os.Chmod(name, 0o640); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, m.sidecarPath(s.ID)); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

func (m *Manager) removeFiles(s *stored) {
	e := &entry{stored: *s}
	if p := m.pathOf(e); p != "" {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			m.log.Warn("remove recording file", "id", s.ID, "err", err)
		}
	}
	if ValidID(s.ID) {
		_ = os.Remove(m.sidecarPath(s.ID))
	}
}

// load reads the sidecars of earlier runs. A recording that was still active
// when the previous process died is marked failed (its file is kept when it
// holds data, removed when empty) and stray temp files are deleted.
func (m *Manager) load() {
	ents, err := os.ReadDir(m.cfg.Dir)
	if err != nil {
		m.log.Warn("read recordings dir", "err", err)
		return
	}
	for _, de := range ents {
		name := de.Name()
		if strings.HasPrefix(name, ".rec-") && strings.HasSuffix(name, ".tmp") {
			_ = os.Remove(filepath.Join(m.cfg.Dir, name))
			continue
		}
		if de.IsDir() || !strings.HasSuffix(name, sidecarSuffix) {
			continue
		}
		id := strings.TrimSuffix(name, sidecarSuffix)
		if !ValidID(id) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(m.cfg.Dir, name))
		if err != nil {
			continue
		}
		var s stored
		if json.Unmarshal(b, &s) != nil || s.ID != id {
			continue
		}
		e := &entry{stored: s, finished: make(chan struct{}), stopCh: make(chan struct{})}
		close(e.finished)
		if m.pathOf(e) == "" {
			continue
		}
		st, serr := os.Stat(m.pathOf(e))
		if e.active() {
			e.Status = StatusFailed
			e.Err = "interrupted by server restart"
			e.StoppedAt = m.cfg.Now().UTC()
			if serr == nil {
				e.Size = st.Size()
				e.Seconds = int64(e.StoppedAt.Sub(e.StartedAt) / time.Second)
			}
			if serr != nil || e.Size == 0 {
				_ = os.Remove(m.pathOf(e))
				e.Size = 0
			}
			_ = m.writeSidecar(&e.stored)
		} else if serr == nil {
			e.Size = st.Size()
		}
		m.recs[id] = e
	}
}

// ---- retention ------------------------------------------------------------

func (m *Manager) janitor() {
	defer m.wg.Done()
	t := time.NewTicker(m.cfg.SweepEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			m.sweep()
		case <-m.stopJan:
			return
		}
	}
}

// sweep deletes finished recordings older than the retention period.
func (m *Manager) sweep() {
	if m.cfg.Retention <= 0 {
		return
	}
	cutoff := m.cfg.Now().Add(-m.cfg.Retention)
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, e := range m.recs {
		if e.active() {
			continue
		}
		t := e.StoppedAt
		if t.IsZero() {
			t = e.StartedAt
		}
		if t.Before(cutoff) {
			m.log.Info("retention: deleting recording", "id", id)
			m.removeFiles(&e.stored)
			delete(m.recs, id)
		}
	}
}

// ---- exec runner ----------------------------------------------------------

type execProc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	tail  *tailBuffer
}

func execStart(bin string, args []string) (Proc, error) {
	cmd := exec.Command(bin, args...)
	tb := &tailBuffer{max: stderrTail}
	cmd.Stderr = tb
	cmd.Stdout = io.Discard
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProc{cmd: cmd, stdin: in, tail: tb}, nil
}

func (p *execProc) Wait() error { return p.cmd.Wait() }

func (p *execProc) Quit() {
	_, _ = io.WriteString(p.stdin, "q")
	_ = p.stdin.Close()
}

func (p *execProc) Kill() {
	if p.cmd.Process == nil {
		return
	}
	if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
		_ = p.cmd.Process.Kill()
		return
	}
	time.AfterFunc(3*time.Second, func() { _ = p.cmd.Process.Kill() })
}

func (p *execProc) Output() string { return p.tail.String() }

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append(t.b[:0], t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
