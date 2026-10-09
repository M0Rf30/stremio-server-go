// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package streamproxy

import (
	"container/list"
	"context"
	"errors"
	"sync"
)

// defaultInflightMaxBytes caps the segment bytes that may be buffered in
// memory at once while an upstream response is still being read (cache fills,
// prefetches and DRM decrypts). The segment cache's own byte budget only
// counts entries that are already stored, so without this cap N concurrent
// fetches of up to maxSegmentBytes each would sit entirely outside any budget.
// It must be at least maxSegmentBytes so a single maximum-size segment can
// always be admitted.
const defaultInflightMaxBytes = 2 * maxSegmentBytes

var (
	// errInflightBusy reports that the in-flight buffering budget is
	// exhausted and the caller chose not to wait for it.
	errInflightBusy = errors.New("in-flight segment budget exhausted")
	// errBudgetTooBig reports a request larger than the whole budget, which
	// could never be granted.
	errBudgetTooBig = errors.New("request exceeds in-flight segment budget")
)

// byteBudget is a FIFO weighted semaphore over bytes. A nil *byteBudget is an
// unlimited budget (every acquire succeeds immediately), so a Handler built
// without New still works. Callers must release exactly what they acquired.
type byteBudget struct {
	mu      sync.Mutex
	max     int64
	used    int64
	peak    int64 // high-water mark of used
	waiters list.List
}

// budgetWaiter is one blocked acquire; ready is closed once its n bytes have
// been granted.
type budgetWaiter struct {
	n     int64
	ready chan struct{}
}

// newByteBudget returns a budget of limit bytes.
func newByteBudget(limit int64) *byteBudget {
	return &byteBudget{max: limit}
}

// tryAcquire reserves n bytes without blocking. It fails when the bytes are
// not free, when earlier callers are already queued (keeps waiters FIFO), or
// when n exceeds the whole budget.
func (b *byteBudget) tryAcquire(n int64) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > b.max || b.used+n > b.max || b.waiters.Len() > 0 {
		return false
	}
	b.grant(n)
	return true
}

// acquire reserves n bytes, blocking until they are free or ctx is done.
func (b *byteBudget) acquire(ctx context.Context, n int64) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	if n > b.max {
		b.mu.Unlock()
		return errBudgetTooBig
	}
	if b.used+n <= b.max && b.waiters.Len() == 0 {
		b.grant(n)
		b.mu.Unlock()
		return nil
	}
	w := &budgetWaiter{n: n, ready: make(chan struct{})}
	el := b.waiters.PushBack(w)
	b.mu.Unlock()

	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
		b.mu.Lock()
		select {
		case <-w.ready:
			// Granted just as ctx fired: hand the bytes straight back.
			b.used -= n
		default:
			b.waiters.Remove(el)
		}
		// A departing head-of-line waiter may have been blocking smaller
		// ones behind it.
		b.notifyLocked()
		b.mu.Unlock()
		return ctx.Err()
	}
}

// release returns n previously acquired bytes and wakes queued waiters.
func (b *byteBudget) release(n int64) {
	if b == nil || n <= 0 {
		return
	}
	b.mu.Lock()
	b.used -= n
	b.notifyLocked()
	b.mu.Unlock()
}

// grant records n bytes as used. Caller holds b.mu.
func (b *byteBudget) grant(n int64) {
	b.used += n
	if b.used > b.peak {
		b.peak = b.used
	}
}

// notifyLocked grants queued waiters in FIFO order while they fit.
func (b *byteBudget) notifyLocked() {
	for {
		el := b.waiters.Front()
		if el == nil {
			return
		}
		w := el.Value.(*budgetWaiter)
		if b.used+w.n > b.max {
			return
		}
		b.grant(w.n)
		b.waiters.Remove(el)
		close(w.ready)
	}
}
