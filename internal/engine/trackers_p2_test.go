// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package engine

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

func deadHTTPURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	return u
}

// TestRankAndKeepDropsFailedProbes: dead trackers must not fill spare slots.
func TestRankAndKeepDropsFailedProbes(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer live.Close()
	dead := deadHTTPURL(t)

	got := rankAndKeep([]string{dead, live.URL, dead + "/x"}, 5, "")
	if !reflect.DeepEqual(got, []string{live.URL}) {
		t.Fatalf("got %v, want only the live tracker", got)
	}
	if got := rankAndKeep([]string{dead}, 5, ""); len(got) != 0 {
		t.Fatalf("all-dead: got %v, want empty", got)
	}
}

// TestDoRefreshTrackersKeepsListWhenNoneRespond: nothing succeeded → keep the
// in-memory list and do not overwrite the persisted cache.
func TestDoRefreshTrackersKeepsListWhenNoneRespond(t *testing.T) {
	orig := getTrackers()
	t.Cleanup(func() { setTrackers(orig) })
	want := []string{"udp://keep.example:6969/announce"}
	setTrackers(want)

	dead := deadHTTPURL(t)
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(dead + "/announce\n"))
	}))
	defer src.Close()

	cache := filepath.Join(t.TempDir(), "trackers_best.txt")
	doRefreshTrackers(cache, 5, src.URL, newTrackerClient(""), "")

	if got := getTrackers(); !reflect.DeepEqual(got, want) {
		t.Fatalf("tracker list replaced: got %v, want %v", got, want)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatalf("cache must not be written when nothing responded (stat err=%v)", err)
	}
}

// TestProbeTrackerHTTPDisablesKeepAlive: probes must ask the server to close.
func TestProbeTrackerHTTPDisablesKeepAlive(t *testing.T) {
	var closed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		closed.Store(r.Close)
	}))
	defer srv.Close()
	if rtt := probeTrackerHTTP(srv.URL, ""); rtt >= probeMaxRTT {
		t.Fatal("probe failed")
	}
	if !closed.Load() {
		t.Fatal("probe request did not send Connection: close (keep-alives enabled)")
	}
}

// TestRankWSSBoundsConcurrency: never more than maxWSSProbeConcurrency
// handshakes in flight.
func TestRankWSSBoundsConcurrency(t *testing.T) {
	orig := probeWSFn
	t.Cleanup(func() { probeWSFn = orig })

	var cur, peak atomic.Int32
	release := make(chan struct{})
	full := make(chan struct{})
	var fullOnce sync.Once
	probeWSFn = func(string) bool {
		c := cur.Add(1)
		for {
			p := peak.Load()
			if c <= p || peak.CompareAndSwap(p, c) {
				break
			}
		}
		if c >= maxWSSProbeConcurrency {
			fullOnce.Do(func() { close(full) })
		}
		<-release
		cur.Add(-1)
		return true
	}

	n := maxWSSProbeConcurrency * 3
	list := make([]string, n)
	for i := range list {
		list[i] = fmt.Sprintf("wss://t%d.example", i)
	}
	done := make(chan []string, 1)
	go func() { done <- rankWSS(list) }()

	<-full
	for range 200 { // give any unbounded extra goroutines a chance to start
		runtime.Gosched()
	}
	close(release)
	live := <-done

	if len(live) != n {
		t.Fatalf("live=%d want %d", len(live), n)
	}
	if got := peak.Load(); got > maxWSSProbeConcurrency {
		t.Fatalf("peak concurrency %d > bound %d", got, maxWSSProbeConcurrency)
	}
}
