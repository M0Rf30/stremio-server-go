// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package media

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"
)

// BenchmarkRelayRegisterAtCapacity measures registering a fresh upstream URL
// while the token table is full (the steady state of a long-running server
// rewriting playlists): every call has to make room first.
func BenchmarkRelayRegisterAtCapacity(b *testing.B) {
	r := newMediaRelay(false)
	for i := range relayMaxTokens {
		if _, err := r.register("http://93.184.216.34/fill" + strconv.Itoa(i) + ".ts"); err != nil {
			b.Fatal(err)
		}
	}
	urls := make([]string, b.N)
	for i := range urls {
		urls[i] = "http://93.184.216.34/new" + strconv.Itoa(i) + ".ts"
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if _, err := r.register(urls[i]); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRelayRegisterReuse measures the dedupe path (an already-registered
// URL being re-fetched) with a full table.
func BenchmarkRelayRegisterReuse(b *testing.B) {
	r := newMediaRelay(false)
	urls := make([]string, relayMaxTokens)
	for i := range urls {
		urls[i] = "http://93.184.216.34/fill" + strconv.Itoa(i) + ".ts"
		if _, err := r.register(urls[i]); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if _, err := r.register(urls[i%len(urls)]); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTranscodeSlotContended has far more goroutines than slots, each
// repeatedly acquiring, holding briefly and releasing one: exercises the
// queueing/wake path.
func BenchmarkTranscodeSlotContended(b *testing.B) {
	m := &hlsManager{cfg: HLSConfig{SegmentConcurrency: 2}}
	ctx := context.Background()
	b.ReportAllocs()
	b.SetParallelism(8)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := m.acquireTranscodeSlot(ctx); err != nil {
				b.Error(err)
				return
			}
			spin(5 * time.Microsecond) // stand-in for work done under the slot
			m.releaseTranscodeSlot()
		}
	})
}

// spin busy-waits for d (timer-based sleeps are far coarser than the hold
// times being modelled).
func spin(d time.Duration) {
	for end := time.Now().Add(d); time.Now().Before(end); {
	}
}

// BenchmarkLockForParallel measures the per-segment-request lock lookup when
// every segment already has its mutex (the cache-hit path).
func BenchmarkLockForParallel(b *testing.B) {
	s := &hlsSession{segLocks: map[string]*sync.Mutex{}}
	names := make([]string, 64)
	for i := range names {
		names[i] = fmt.Sprintf("seg%d.ts", i)
		s.lockFor(names[i])
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			_ = s.lockFor(names[i&63])
			i++
		}
	})
}

// BenchmarkTranscodeSlotHandoff measures the latency between a slot being
// released and a blocked waiter acquiring it (concurrency 1). A polling
// implementation shows up here as up to its poll interval per handoff.
func BenchmarkTranscodeSlotHandoff(b *testing.B) {
	m := &hlsManager{cfg: HLSConfig{SegmentConcurrency: 1}}
	ctx := context.Background()
	got := make(chan time.Time)
	var total time.Duration
	b.ResetTimer()
	for range b.N {
		if err := m.acquireTranscodeSlot(ctx); err != nil {
			b.Fatal(err)
		}
		go func() {
			if err := m.acquireTranscodeSlot(ctx); err != nil {
				b.Error(err)
			}
			got <- time.Now()
		}()
		time.Sleep(2 * time.Millisecond) // let the waiter block
		t0 := time.Now()
		m.releaseTranscodeSlot()
		total += (<-got).Sub(t0)
		m.releaseTranscodeSlot()
	}
	b.ReportMetric(float64(total.Nanoseconds())/float64(b.N), "handoff-ns")
}
