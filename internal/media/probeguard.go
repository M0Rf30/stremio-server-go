// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package media

import (
	"context"
	"net/url"
	"strings"
	"sync"

	"golang.org/x/sync/singleflight"
)

// maxConcurrentProbes bounds how many ffprobe / ranged-HTTP probes (Probe,
// Tracks, OpenSubHash, SubtitlesTracks) run at once, so a burst of distinct
// URLs cannot fork-bomb the host.
const maxConcurrentProbes = 4

// probeGuard combines a concurrency semaphore with per-key singleflight. The
// zero value is ready to use.
type probeGuard struct {
	once sync.Once
	sem  chan struct{}
	sf   singleflight.Group
}

func (g *probeGuard) init() {
	g.once.Do(func() { g.sem = make(chan struct{}, maxConcurrentProbes) })
}

// acquire blocks for a probe slot or until ctx is done.
func (g *probeGuard) acquire(ctx context.Context) error {
	g.init()
	select {
	case g.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *probeGuard) release() { <-g.sem }

// do runs fn once per in-flight key. fn runs with a context detached from any
// single caller's cancellation (so one client hanging up does not fail the
// probe other callers share); every caller still returns as soon as its own
// ctx is done. fn bounds its own run time.
func (g *probeGuard) do(ctx context.Context, key string, fn func(ctx context.Context) (interface{}, error)) (interface{}, error) {
	g.init()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ch := g.sf.DoChan(key, func() (interface{}, error) {
		return fn(context.WithoutCancel(ctx))
	})
	select {
	case r := <-ch:
		return r.Val, r.Err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// normalizeProbeKey canonicalizes a URL so trivially different spellings
// (scheme/host case, default port, fragment) share one in-flight probe.
func normalizeProbeKey(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return strings.TrimSpace(raw)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Fragment = ""
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	if strings.Contains(u.Hostname(), ":") {
		host = "[" + strings.ToLower(u.Hostname()) + "]" + strings.TrimPrefix(host, strings.ToLower(u.Hostname()))
	}
	u.Host = host
	return u.String()
}
