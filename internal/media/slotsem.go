// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package media

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// slotRecheckInterval is how often a blocked acquirer re-reads the (live,
// /settings-overridable) concurrency limit. Releases wake waiters directly;
// this slow tick exists only so a limit that is raised while every slot is
// busy still takes effect without waiting for the next release.
const slotRecheckInterval = 250 * time.Millisecond

// slotSem is a FIFO counting semaphore whose limit is supplied on every call,
// so it can be resized live (the limit comes from /settings) without
// rebuilding the primitive. Waiters block on a per-waiter channel closed by
// release — there is no polling on the hot path. The zero value is ready to
// use.
type slotSem struct {
	mu      sync.Mutex
	held    int
	waiters list.List // of *slotWaiter, oldest first
}

type slotWaiter struct {
	ready   chan struct{} // closed when the slot has been handed over
	granted bool          // guarded by slotSem.mu
}

// grantLocked hands free slots (per limit) to waiters in arrival order.
func (s *slotSem) grantLocked(limit int) {
	for s.held < limit {
		front := s.waiters.Front()
		if front == nil {
			return
		}
		w := s.waiters.Remove(front).(*slotWaiter)
		w.granted = true
		s.held++
		close(w.ready)
	}
}

// acquire blocks until a slot is available under limit (re-evaluated via
// limitFn while blocked) or ctx is done. limitFn is invoked outside the lock.
func (s *slotSem) acquire(ctx context.Context, limitFn func() int) error {
	limit := limitFn()
	s.mu.Lock()
	s.grantLocked(limit) // a raised limit may free slots for queued waiters first
	if s.held < limit && s.waiters.Len() == 0 {
		s.held++
		s.mu.Unlock()
		return nil
	}
	w := &slotWaiter{ready: make(chan struct{})}
	el := s.waiters.PushBack(w)
	s.mu.Unlock()

	tick := time.NewTicker(slotRecheckInterval)
	defer tick.Stop()
	for {
		select {
		case <-w.ready:
			return nil
		case <-ctx.Done():
			// Re-read the live limit (outside the lock): it may have been
			// lowered since this waiter queued, and the hand-back below must
			// not over-grant against a stale value.
			limit = limitFn()
			s.mu.Lock()
			if w.granted {
				// Lost the race: a release already handed us a slot. Give
				// it straight back so it is not leaked.
				s.held--
				s.grantLocked(limit)
			} else {
				s.waiters.Remove(el)
			}
			s.mu.Unlock()
			return ctx.Err()
		case <-tick.C:
			limit = limitFn()
			s.mu.Lock()
			s.grantLocked(limit)
			s.mu.Unlock()
		}
	}
}

// release returns a slot and wakes the next waiter(s) if the limit allows.
func (s *slotSem) release(limitFn func() int) {
	s.mu.Lock()
	if s.held > 0 {
		s.held--
	}
	queued := s.waiters.Len() > 0
	s.mu.Unlock()
	if !queued {
		return // nobody to wake: skip the (settings-mutex) limit lookup
	}
	limit := limitFn()
	s.mu.Lock()
	s.grantLocked(limit)
	s.mu.Unlock()
}
