// Package api — regression tests for the in-flight refcount guard on the nzb
// and archive session janitors (see nzb.go/nzbEvictIdle and
// archive.go/archiveEvict): a session with a non-zero refcount must never be
// evicted, no matter how stale lastAccess is, and becomes evictable again the
// moment the refcount drops back to zero.
package api

import (
	"testing"
	"time"
)

func TestEvictIdle_SkipsInFlightSession(t *testing.T) {
	t.Run("nzb", func(t *testing.T) {
		sess := &nzbSession{
			key:        "refcount-nzb",
			tmpDir:     t.TempDir(),
			lastAccess: time.Now().Add(-2 * time.Hour), // well past the 1h TTL
			refCount:   1,                              // request still in flight
			fileStates: map[string]*nzbFileState{},
		}
		nzbSessionsMu.Lock()
		nzbSessions[sess.key] = sess
		nzbSessionsMu.Unlock()

		nzbEvictIdle()
		nzbSessionsMu.Lock()
		_, present := nzbSessions[sess.key]
		nzbSessionsMu.Unlock()
		if !present {
			t.Fatal("session with refCount > 0 must not be evicted despite stale lastAccess")
		}

		nzbSessionsMu.Lock()
		sess.refCount = 0
		nzbSessionsMu.Unlock()

		nzbEvictIdle()
		nzbSessionsMu.Lock()
		_, present = nzbSessions[sess.key]
		nzbSessionsMu.Unlock()
		if present {
			t.Fatal("session must be evicted once refCount drops to 0 and TTL has elapsed")
		}
	})

	t.Run("archive", func(t *testing.T) {
		sess := &archiveSession{
			key:        "refcount-archive",
			tmpDir:     t.TempDir(),
			lastAccess: time.Now().Add(-2 * time.Hour), // well past the 1h TTL
			refCount:   1,                              // request still in flight
			extracted:  map[string]string{},
		}
		archiveSessionsMu.Lock()
		archiveSessions[sess.key] = sess
		archiveSessionsMu.Unlock()

		archiveEvict()
		archiveSessionsMu.Lock()
		_, present := archiveSessions[sess.key]
		archiveSessionsMu.Unlock()
		if !present {
			t.Fatal("session with refCount > 0 must not be evicted despite stale lastAccess")
		}

		sess.mu.Lock()
		sess.refCount = 0
		sess.mu.Unlock()

		archiveEvict()
		archiveSessionsMu.Lock()
		_, present = archiveSessions[sess.key]
		archiveSessionsMu.Unlock()
		if present {
			t.Fatal("session must be evicted once refCount drops to 0 and TTL has elapsed")
		}
	})
}
