// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package media

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// waitSemWaiters spins (bounded) until the semaphore has n queued waiters.
func waitSemWaiters(t *testing.T, s *slotSem, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		got := s.waiters.Len()
		s.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiters = %d, want %d", got, n)
		}
		runtime.Gosched()
	}
}

// TestSlotSemCancelAfterGrantUsesLiveLimit: a waiter granted a slot while its
// ctx is cancelling hands the slot back; that hand-back must re-read the live
// limit (lowered via /settings in the meantime), not the stale one captured
// when it started waiting, or the next waiters are over-granted.
func TestSlotSemCancelAfterGrantUsesLiveLimit(t *testing.T) {
	var limit atomic.Int32
	limit.Store(2)
	limitFn := func() int { return int(limit.Load()) }

	var s slotSem
	bg := context.Background()
	// Two holders fill the semaphore at limit 2.
	for range 2 {
		if err := s.acquire(bg, limitFn); err != nil {
			t.Fatal(err)
		}
	}

	// W (cancellable), X and Y queue up behind them, in that order.
	wCtx, wCancel := context.WithCancel(bg)
	defer wCancel()
	wDone := make(chan error, 1)
	go func() { wDone <- s.acquire(wCtx, limitFn) }()
	waitSemWaiters(t, &s, 1)

	xyCtx, xyCancel := context.WithCancel(bg)
	defer xyCancel()
	for i := 2; i <= 3; i++ {
		go func() { _ = s.acquire(xyCtx, limitFn) }()
		waitSemWaiters(t, &s, i)
	}

	// Operator lowers the limit to 1.
	limit.Store(1)

	// Deterministically reproduce the race outcome: under the lock, cancel W's
	// ctx (the only ready channel, so W takes the ctx.Done branch) and mark W
	// as already granted (as a concurrent release would), with the remaining
	// state being "W holds the only slot".
	s.mu.Lock()
	wCancel()
	front := s.waiters.Front()
	w := s.waiters.Remove(front).(*slotWaiter)
	w.granted = true
	s.held = 1
	s.mu.Unlock()

	if err := <-wDone; err == nil {
		t.Fatal("W acquire returned nil, want ctx error")
	}

	s.mu.Lock()
	held := s.held
	s.mu.Unlock()
	if held > limitFn() {
		t.Fatalf("held = %d exceeds live limit %d after cancelled-grant hand-back", held, limitFn())
	}
}
