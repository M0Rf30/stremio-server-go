// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package media

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/types"
)

// relayGet fetches relayURL and drains the body, ignoring errors (a broken
// upstream body aborts the relay's response).
func relayGet(t *testing.T, relayURL string) {
	t.Helper()
	resp, err := http.Get(relayURL) //nolint:gosec // our own loopback listener
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func TestMediaRelayRecordsUpstreamFailure(t *testing.T) {
	cases := []struct {
		name        string
		handler     http.HandlerFunc
		wantReason  string // "" = no failure recorded
		wantAlt     string // another acceptable reason ("" = none): see the body case
		wantTimeout bool
	}{
		{"healthy", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "data") }, "", "", false},
		{"rate limited", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "slow down", http.StatusTooManyRequests)
		}, "upstream returned 429 Too Many Requests", "", false},
		{"read past the end (416) is not a failure", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "range", http.StatusRequestedRangeNotSatisfiable)
		}, "", "", false},
		{"gateway timeout", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "late", http.StatusGatewayTimeout)
		}, "upstream returned 504 Gateway Timeout", "", true},
		{"body breaks off", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, strings.Repeat("x", 1024))
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
			// A client that has not yet read the headers when the abort arrives
			// records a failed fetch instead (seen on Windows); both are source
			// failures.
		}, "upstream body ended early", "upstream fetch failed", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(tc.handler)
			defer upstream.Close()
			r := newMediaRelay(false)
			relayURL, err := r.register(upstream.URL + "/secret-token/a.mkv")
			if err != nil {
				t.Fatal(err)
			}
			began := time.Now()
			relayGet(t, relayURL)
			f, ok := r.failureSince(relayURL, began)
			wantOK := tc.wantReason != ""
			reasonOK := f.reason == tc.wantReason || (tc.wantAlt != "" && f.reason == tc.wantAlt)
			if ok != wantOK || (wantOK && !reasonOK) || f.timeout != tc.wantTimeout {
				t.Fatalf("failureSince = %+v, %v; want reason %q (or %q) timeout %v", f, ok, tc.wantReason, tc.wantAlt, tc.wantTimeout)
			}
			if strings.Contains(f.reason, "secret-token") {
				t.Errorf("reason %q leaks the upstream URL", f.reason)
			}
			if _, ok := r.failureSince(relayURL, time.Now().Add(time.Second)); ok {
				t.Error("a failure older than since was reported")
			}
		})
	}
}

func TestMediaRelayFetchFailure(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	dead := upstream.URL
	upstream.Close() // connection refused from now on
	r := newMediaRelay(false)
	relayURL, err := r.register(dead + "/a.mkv")
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	relayGet(t, relayURL)
	if f, ok := r.failureSince(relayURL, began); !ok || f.reason != "upstream fetch failed" || f.timeout {
		t.Errorf("failureSince = %+v, %v; want a non-timeout fetch failure", f, ok)
	}
	if _, ok := r.failureSince("http://127.0.0.1:1/not-a-relay-url", began); ok {
		t.Error("failureSince matched a URL that is not a relay token")
	}
}

func TestClassifySegmentError(t *testing.T) {
	r := newMediaRelay(false)
	relayURL, err := r.register("http://93.184.216.34/a.mkv")
	if err != nil {
		t.Fatal(err)
	}
	base := errors.New("exit status 1")
	live := context.Background()
	expired, cancel := context.WithDeadline(live, time.Now().Add(-time.Second))
	defer cancel()
	gone, cancelGone := context.WithCancel(live)
	cancelGone()

	began := time.Now()
	if got := classifySegmentError(live, live, r, relayURL, began, base); !unclassified(got, base) {
		t.Errorf("no failure: got %v, want the error unchanged", got)
	}
	if got := classifySegmentError(live, expired, r, relayURL, began, base); !errors.Is(got, types.ErrHLSSourceTimeout) || !errors.Is(got, base) {
		t.Errorf("deadline exceeded: got %v, want ErrHLSSourceTimeout wrapping the cause", got)
	}

	req := httptest.NewRequest(http.MethodGet, relayURL, nil)
	token := strings.TrimPrefix(req.URL.Path, "/r/")
	r.recordFailure(req, token, "upstream returned 429 Too Many Requests", false)
	got := classifySegmentError(live, live, r, relayURL, began, base)
	if !errors.Is(got, types.ErrHLSSourceFailed) || !strings.Contains(got.Error(), "429") {
		t.Errorf("origin 429: got %v, want ErrHLSSourceFailed with the reason", got)
	}
	if got := classifySegmentError(gone, live, r, relayURL, began, base); !unclassified(got, base) {
		t.Errorf("caller gone: got %v, want the error unchanged", got)
	}
	r.recordFailure(req, token, "upstream timed out", true)
	if got := classifySegmentError(live, live, r, relayURL, began, base); !errors.Is(got, types.ErrHLSSourceTimeout) {
		t.Errorf("origin timeout: got %v, want ErrHLSSourceTimeout", got)
	}

	canceledReq := httptest.NewRequest(http.MethodGet, relayURL, nil).WithContext(gone)
	r2 := newMediaRelay(false)
	relay2, _ := r2.register("http://93.184.216.34/b.mkv")
	r2.recordFailure(canceledReq, strings.TrimPrefix(canceledReq.URL.Path, "/r/"), "upstream fetch failed", false)
	if _, ok := r2.failureSince(relay2, began); ok {
		t.Error("a failure seen after ffmpeg cancelled its own request was recorded")
	}
}

// unclassified reports whether got is base without a source-failure mark.
func unclassified(got, base error) bool {
	return errors.Is(got, base) && !errors.Is(got, types.ErrHLSSourceFailed) && !errors.Is(got, types.ErrHLSSourceTimeout)
}
