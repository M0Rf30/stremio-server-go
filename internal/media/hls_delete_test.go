// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package media

// Tests for explicit HLS session deletion (DeleteHLS) and for idle eviction
// being disabled (HLSConfig.DisableIdleEviction, STREMIO_HLS_SESSION_TTL=0).
// All offline: sessions are injected directly and the only HLSFile paths
// exercised are ones served without ffmpeg (playlists, an already-extracted
// sub<k>.vtt).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/types"
)

// injectSession registers a playlist-capable session named id (with its
// directory created on disk) and returns it.
func injectSession(t *testing.T, m *hlsManager, id string) *hlsSession {
	t.Helper()
	dir := filepath.Join(m.base, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := &hlsSession{
		dir:             dir,
		duration:        8,
		segLocks:        map[string]*sync.Mutex{},
		playlistData:    map[string]struct{}{},
		subtitleStreams: []subtitleStream{{Index: 2, SubIdx: 0, CodecName: "subrip"}},
	}
	s.lastAccess.Store(time.Now().UnixNano())
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	return s
}

func dirExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func TestDeleteHLSRemovesSessionAndDir(t *testing.T) {
	m := newTestHLSManager(t)
	s := injectSession(t, m, "sess")
	if _, _, err := m.HLSFile(context.Background(), "sess", "playlist.m3u8"); err != nil {
		t.Fatalf("HLSFile(playlist.m3u8) before delete = %v", err)
	}

	if err := m.DeleteHLS("sess"); err != nil {
		t.Fatalf("DeleteHLS = %v, want nil", err)
	}
	if n := m.Sessions(); n != 0 {
		t.Errorf("Sessions() after delete = %d, want 0", n)
	}
	if dirExists(s.dir) {
		t.Error("session dir still exists after deleting an idle session")
	}
	m.mu.Lock()
	nDraining := len(m.draining)
	m.mu.Unlock()
	if nDraining != 0 {
		t.Errorf("draining has %d entries after an idle delete, want 0", nDraining)
	}
	_, _, err := m.HLSFile(context.Background(), "sess", "playlist.m3u8")
	if !errors.Is(err, types.ErrHLSSessionNotFound) {
		t.Errorf("HLSFile after delete = %v, want ErrHLSSessionNotFound", err)
	}
	// A second delete of the same id is a plain not-found.
	if err := m.DeleteHLS("sess"); !errors.Is(err, types.ErrHLSSessionNotFound) {
		t.Errorf("second DeleteHLS = %v, want ErrHLSSessionNotFound", err)
	}
}

func TestDeleteHLSErrors(t *testing.T) {
	m := newTestHLSManager(t)
	injectSession(t, m, "keep")
	cases := []struct {
		id   string
		want error
	}{
		{"missing", types.ErrHLSSessionNotFound},
		{"", types.ErrHLSInvalidSessionID},
		{".", types.ErrHLSInvalidSessionID},
		{"..", types.ErrHLSInvalidSessionID},
		{"../keep", types.ErrHLSInvalidSessionID},
		{"foo/bar", types.ErrHLSInvalidSessionID},
		{"foo..bar", types.ErrHLSInvalidSessionID},
		{"/etc/passwd", types.ErrHLSInvalidSessionID},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("id=%q", c.id), func(t *testing.T) {
			if err := m.DeleteHLS(c.id); !errors.Is(err, c.want) {
				t.Errorf("DeleteHLS(%q) = %v, want %v", c.id, err, c.want)
			}
		})
	}
	if n := m.Sessions(); n != 1 {
		t.Errorf("failed deletes changed the session count: %d, want 1", n)
	}
}

// TestHLSErrorTextUnchanged pins the wire text of the two errors that now wrap
// sentinels: handleHLS writes err.Error() into the response body, so wrapping
// must not change what clients see.
func TestHLSErrorTextUnchanged(t *testing.T) {
	m := newTestHLSManager(t)
	_, _, err := m.HLSFile(context.Background(), "nope", "playlist.m3u8")
	if err == nil || err.Error() != "hls: unknown session nope" {
		t.Errorf("HLSFile unknown-session error = %v, want %q", err, "hls: unknown session nope")
	}
	_, err = m.StartHLS("..", "http://93.184.216.34/dummy.mkv", types.HLSSessionOptions{})
	if err == nil || err.Error() != `hls: invalid session id ".."` {
		t.Errorf("StartHLS invalid-id error = %v, want %q", err, `hls: invalid session id ".."`)
	}
}

// TestDeleteHLSDefersRemovalWhileInFlight drives a real HLSFile call that is
// blocked mid-flight (on the sub0.vtt per-file lock, the same lock a running
// subtitle extraction holds), deletes the session under it, and checks that
// the directory survives until that call returns and is removed right after.
func TestDeleteHLSDefersRemovalWhileInFlight(t *testing.T) {
	m := newTestHLSManager(t)
	s := injectSession(t, m, "busy")

	lock := s.lockFor("sub0.vtt")
	lock.Lock()
	type result struct {
		path string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		p, _, err := m.HLSFile(context.Background(), "busy", "sub0.vtt")
		done <- result{p, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for s.inFlight.Load() != 1 {
		if time.Now().After(deadline) {
			lock.Unlock()
			t.Fatal("HLSFile never became in-flight")
		}
		time.Sleep(time.Millisecond)
	}

	if err := m.DeleteHLS("busy"); err != nil {
		lock.Unlock()
		t.Fatalf("DeleteHLS = %v, want nil", err)
	}
	if !dirExists(s.dir) {
		lock.Unlock()
		t.Fatal("session dir removed while an HLSFile call was still in flight")
	}
	// Unregistered immediately: new requests see an unknown session...
	if _, _, err := m.HLSFile(context.Background(), "busy", "playlist.m3u8"); !errors.Is(err, types.ErrHLSSessionNotFound) {
		t.Errorf("HLSFile on a deleted (draining) session = %v, want ErrHLSSessionNotFound", err)
	}
	// ...and the id cannot be re-created on top of the directory that is
	// about to be removed. (Public IP literal: passes validateRemoteURL with
	// no DNS lookup, and StartHLS returns before any probing.)
	if _, err := m.StartHLS("busy", "http://93.184.216.34/dummy.mkv", types.HLSSessionOptions{}); err == nil || !strings.Contains(err.Error(), "being deleted") {
		t.Errorf("StartHLS on a draining id = %v, want a 'being deleted' error", err)
	}

	// Let the in-flight call finish: give it an already-extracted subtitle so
	// extractSubtitle returns the cached file without running ffmpeg.
	if err := os.WriteFile(filepath.Join(s.dir, "sub0.vtt"), []byte("WEBVTT\n"), 0o644); err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	lock.Unlock()
	r := <-done
	if r.err != nil {
		t.Fatalf("in-flight HLSFile = %v, want nil (it started before the delete)", r.err)
	}

	if dirExists(s.dir) {
		t.Error("session dir not removed after the last in-flight call returned")
	}
	m.mu.Lock()
	_, stillDraining := m.draining["busy"]
	m.mu.Unlock()
	if stillDraining {
		t.Error("id still marked draining after its directory was removed")
	}
}

// TestRemoveDeletedSessionRunsOnce covers the case where DeleteHLS and the
// last in-flight exit both observe inFlight == 0: the second removal must be
// a no-op, so it cannot wipe a directory recreated in between by a new
// session with the same id.
func TestRemoveDeletedSessionRunsOnce(t *testing.T) {
	m := newTestHLSManager(t)
	s := injectSession(t, m, "twice")
	if err := m.DeleteHLS("twice"); err != nil {
		t.Fatalf("DeleteHLS = %v", err)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m.removeDeletedSession("twice", s)
	if !dirExists(s.dir) {
		t.Error("second removeDeletedSession call removed the directory again")
	}
}

func TestCloseHLSClearsDraining(t *testing.T) {
	m := newTestHLSManager(t)
	s := injectSession(t, m, "held")
	s.inFlight.Add(1)
	if err := m.DeleteHLS("held"); err != nil {
		t.Fatalf("DeleteHLS = %v", err)
	}
	m.CloseHLS()
	m.mu.Lock()
	n := len(m.draining)
	m.mu.Unlock()
	if n != 0 {
		t.Errorf("draining has %d entries after CloseHLS, want 0", n)
	}
	if dirExists(m.base) {
		t.Error("CloseHLS did not remove the base dir")
	}
}

// ── DisableIdleEviction (STREMIO_HLS_SESSION_TTL=0) ─────────────────────────

func TestEvictIdleDisabledNeverEvicts(t *testing.T) {
	m := newTestHLSManager(t)
	m.cfg.DisableIdleEviction = true
	s := injectSession(t, m, "forever")
	s.lastAccess.Store(time.Now().Add(-24 * time.Hour).UnixNano())

	m.evictIdle()

	if n := m.Sessions(); n != 1 {
		t.Fatalf("evictIdle evicted a session with idle eviction disabled (Sessions() = %d)", n)
	}
	if !dirExists(s.dir) {
		t.Fatal("evictIdle removed the session dir with idle eviction disabled")
	}
	// Explicit deletion is still the way out.
	if err := m.DeleteHLS("forever"); err != nil {
		t.Fatalf("DeleteHLS = %v", err)
	}
	if dirExists(s.dir) {
		t.Error("DeleteHLS did not remove the session dir")
	}
}

func TestNormalizeDisableIdleEvictionKeepsSaneReaper(t *testing.T) {
	// What internal/app builds for STREMIO_HLS_SESSION_TTL=0.
	cfg := HLSConfig{
		SessionTTL:          0,
		ReaperInterval:      DefaultReaperInterval(0),
		DisableIdleEviction: true,
	}.normalize(1)
	if !cfg.DisableIdleEviction {
		t.Error("normalize dropped DisableIdleEviction")
	}
	if cfg.ReaperInterval != defaultReaperInterval {
		t.Errorf("ReaperInterval = %s, want %s (never 0 / a busy loop)", cfg.ReaperInterval, defaultReaperInterval)
	}
	if cfg.SessionTTL != defaultSessionTTL {
		t.Errorf("SessionTTL = %s, want the %s default (unused while disabled)", cfg.SessionTTL, defaultSessionTTL)
	}
	// Default config: eviction stays enabled.
	if DefaultHLSConfig().normalize(1).DisableIdleEviction {
		t.Error("DefaultHLSConfig must keep idle eviction enabled")
	}
}
