// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package streamproxy

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/logging"
)

// Embed-page auto-resolution for /proxy/stream (EasyProxy behaviour): when
// the destination matches a definition's "match" pattern it is an embed page,
// not media, so it is resolved with that definition first. Players issue many
// Range requests for one URL, so resolutions are cached briefly.

const (
	embedCacheTTL = 10 * time.Minute
	embedCacheMax = 256
)

type embedEntry struct {
	res     *extractResult
	expires time.Time
}

type embedCache struct {
	mu sync.Mutex
	m  map[string]embedEntry
}

func (c *embedCache) get(key string) (*extractResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || time.Now().After(e.expires) {
		return nil, false
	}
	return e.res, true
}

func (c *embedCache) put(key string, res *extractResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[string]embedEntry)
	}
	if len(c.m) >= embedCacheMax {
		now := time.Now()
		for k, e := range c.m {
			if now.After(e.expires) {
				delete(c.m, k)
			}
		}
		for k := range c.m { // still full: drop an arbitrary entry
			if len(c.m) < embedCacheMax {
				break
			}
			delete(c.m, k)
		}
	}
	c.m[key] = embedEntry{res: res, expires: time.Now().Add(embedCacheTTL)}
}

// resolveEmbed resolves opts.Dest when it matches a definition. It returns
// true when it has written the response (error or redirect to the HLS/MPD
// endpoint); otherwise opts.Dest/ReqHeaders now describe the media to stream.
func (h *Handler) resolveEmbed(w http.ResponseWriter, r *http.Request, opts *Options) bool {
	if h.defs == nil {
		return false
	}
	name, ex, ok := h.defs.match(opts.Dest)
	if !ok {
		return false
	}
	key := name + "\x00" + opts.Proxy + "\x00" + opts.Dest
	res, hit := h.embeds.get(key)
	if !hit {
		ctx, cancel := context.WithTimeout(r.Context(), maxDefTimeout)
		defer cancel()
		var err error
		res, err = ex.run(r.WithContext(ctx), opts.Dest, h.extractorFetch(opts.Proxy))
		if err != nil {
			logging.For("extractor").Warn("embed resolve failed", "extractor", name, "err", err)
			http.Error(w, "embed page could not be resolved", http.StatusBadGateway)
			return true
		}
		if err := h.ValidateDest(res.URL); err != nil {
			http.Error(w, "forbidden destination", http.StatusForbidden)
			return true
		}
		h.embeds.put(key, res)
	}
	// Caller-supplied h_ headers take precedence over the definition's.
	for k, v := range res.Headers {
		if opts.ReqHeaders.Get(k) == "" {
			opts.ReqHeaders.Set(k, v)
		}
	}
	if res.Endpoint != "/proxy/stream" {
		// Playlists must be rewritten by their own endpoint.
		http.Redirect(w, r, h.buildProxyURL(h.externalBase(r), res.Endpoint, res.URL, opts), http.StatusFound)
		return true
	}
	opts.Dest = res.URL
	return false
}
