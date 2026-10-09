// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package streamproxy

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// hlsURIAttrRe matches the URI="..." attribute within an HLS tag line.
var hlsURIAttrRe = regexp.MustCompile(`(?:^|[:,])URI="([^"]*)"`)

// maxManifestBytes is the maximum number of bytes accepted from an upstream
// manifest (HLS playlist or MPEG-DASH MPD). Manifests larger than this are
// rejected to prevent OOM from attacker-controlled upstreams.
const maxManifestBytes = 16 << 20 // 16 MiB

func init() {
	hlsHandler = hlsServe
}

// hlsServe handles HLS manifest proxy requests at /proxy/hls/manifest.m3u8.
func hlsServe(h *Handler, w http.ResponseWriter, r *http.Request) {
	if err := h.authorize(r); err != nil {
		writeAuthError(w, err)
		return
	}
	opts, err := h.parseOptions(r)
	if err != nil || opts.Dest == "" {
		http.Error(w, "bad request: missing or invalid destination", http.StatusBadRequest)
		return
	}
	if err := h.ValidateDest(opts.Dest); err != nil {
		http.Error(w, "forbidden destination", http.StatusForbidden)
		return
	}
	if h.rejectBlockedProxyHost(w, opts.Proxy) {
		return
	}
	effProxy := opts.Proxy
	if effProxy == "" {
		effProxy = h.cfg.UpstreamProxy
	}
	resp, err := h.fetch(r.Context(), "GET", opts.Dest, opts.ReqHeaders, nil, effProxy)
	if err != nil {
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	// Never rewrite an error page into a bogus playlist: surface upstream
	// failures as a clean 502.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		http.Error(w, "upstream returned status "+strconv.Itoa(resp.StatusCode), http.StatusBadGateway)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		http.Error(w, "reading upstream response failed", http.StatusBadGateway)
		return
	}
	if len(body) > maxManifestBytes {
		http.Error(w, "upstream manifest too large", http.StatusBadGateway)
		return
	}
	// F9: convert body to string once; both hlsSegmentURLs and hlsRewrite share it.
	playlist := string(body)
	if h.cfg.Prebuffer > 0 {
		h.prefetch(context.WithoutCancel(r.Context()), hlsSegmentURLs(opts.Dest, playlist, h.cfg.Prebuffer), opts.ReqHeaders, effProxy)
	}
	rewritten := hlsRewrite(h, r, opts, playlist)
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	setProxySecurityHeaders(w)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(rewritten))
}

// hlsRewrite rewrites all URIs in an HLS playlist so they proxy through this
// server. Processing is line-by-line; all non-URI content is preserved verbatim.
//
// Routing rules:
//   - Bare URI lines (not starting with '#') are proxied through /proxy/stream,
//     except when following an #EXT-X-STREAM-INF tag or when the URL contains
//     ".m3u8", in which case /proxy/hls/manifest.m3u8 is used.
//   - #EXT-X-KEY, #EXT-X-SESSION-KEY, #EXT-X-MAP, #EXT-X-PART and
//     #EXT-X-PRELOAD-HINT URI="..." attributes are always routed to
//     /proxy/stream; all other attributes (METHOD, IV, BYTERANGE, …) are kept.
//   - #EXT-X-RENDITION-REPORT URI="..." is routed to /proxy/hls/manifest.m3u8.
//   - #EXT-X-MEDIA and #EXT-X-I-FRAME-STREAM-INF URI="..." attributes always
//     name a media playlist (RFC 8216 §4.3.4.1, §4.3.4.3) and are routed to
//     /proxy/hls/manifest.m3u8, even when the URL has no ".m3u8" suffix.
//   - URIs with a non-http(s) scheme (data:, skd:, ...) are left untouched.
func hlsRewrite(h *Handler, r *http.Request, opts *Options, playlist string) string {
	lines := strings.Split(playlist, "\n")
	ext := h.externalBase(r)

	// F10: parse opts.Dest once; resolveURL would re-parse it on every call.
	baseURL, baseURLErr := url.Parse(opts.Dest)

	out := make([]string, 0, len(lines))
	nextIsVariant := false // true after #EXT-X-STREAM-INF until next URI line

	for _, line := range lines {
		// Tolerate CRLF playlists (and stray whitespace): classify and resolve
		// on the trimmed line, matching hlsSegmentURLs, so prefetch cache keys
		// equal the URLs the player will request. Blank lines are preserved
		// as empty lines and never turned into proxy URLs.
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			out = append(out, "")
			continue
		}
		line = trimmed

		if !strings.HasPrefix(line, "#") {
			// Bare URI line.
			// F10: inline resolution against pre-parsed baseURL.
			abs := line
			if rv, err := url.Parse(line); err == nil {
				if hlsNonHTTPScheme(rv) {
					nextIsVariant = false
					out = append(out, line)
					continue
				}
				if baseURLErr == nil {
					abs = baseURL.ResolveReference(rv).String()
				}
			}
			ep := "/proxy/stream"
			// F8: avoid strings.ToLower; check both cases of ".m3u8".
			if nextIsVariant || strings.Contains(abs, ".m3u8") || strings.Contains(abs, ".M3U8") {
				ep = "/proxy/hls/manifest.m3u8"
			}
			nextIsVariant = false
			out = append(out, h.buildProxyURL(ext, ep, abs, opts))
			continue
		}

		// F8: HLS tag names are uppercase by RFC 8216; use direct HasPrefix instead
		// of strings.ToUpper(line) to avoid a per-line allocation.
		tag, _, _ := strings.Cut(line, ":")
		switch tag {
		case "#EXT-X-STREAM-INF":
			// URI follows on the next non-comment line.
			nextIsVariant = true
			out = append(out, line)

		case "#EXT-X-KEY", "#EXT-X-SESSION-KEY", "#EXT-X-MAP", "#EXT-X-PART", "#EXT-X-PRELOAD-HINT":
			// Encryption key, initialization segment, LL-HLS part or preload
			// hint: media/key resources, always /proxy/stream.
			out = append(out, hlsRewriteURIAttr(line, baseURL, ext, opts, h, "/proxy/stream"))

		case "#EXT-X-RENDITION-REPORT":
			// LL-HLS rendition report: always a media playlist.
			out = append(out, hlsRewriteURIAttr(line, baseURL, ext, opts, h, "/proxy/hls/manifest.m3u8"))

		case "#EXT-X-MEDIA", "#EXT-X-I-FRAME-STREAM-INF":
			// Alternate rendition or I-frame playlist: always a media playlist.
			out = append(out, hlsRewriteURIAttr(line, baseURL, ext, opts, h, "/proxy/hls/manifest.m3u8"))

		default:
			// All other tags and comments: preserve verbatim, do not reset nextIsVariant
			// so that an intervening comment between #EXT-X-STREAM-INF and the URI is
			// handled correctly.
			out = append(out, line)
		}
	}

	return strings.Join(out, "\n")
}

// hlsRewriteURIAttr rewrites the URI="..." attribute in a single HLS tag line.
// fixedEndpoint is the proxy endpoint to use; if empty, the endpoint is chosen
// automatically: /proxy/hls/manifest.m3u8 when the URL contains ".m3u8",
// /proxy/stream otherwise.
// baseURL is the pre-parsed base URL from hlsRewrite (F10); may be nil on parse error.
// URIs with a non-http(s) scheme (data:, skd:, urn:, ...) and empty URIs are
// left untouched.
func hlsRewriteURIAttr(line string, baseURL *url.URL, ext string, opts *Options, h *Handler, fixedEndpoint string) string {
	loc := hlsURIAttrRe.FindStringSubmatchIndex(line)
	if loc == nil {
		return line
	}
	ref := line[loc[2]:loc[3]]
	if ref == "" {
		return line
	}
	// F10: inline resolution against pre-parsed baseURL.
	abs := ref
	rv, err := url.Parse(ref)
	if err == nil {
		if hlsNonHTTPScheme(rv) {
			return line
		}
		if baseURL != nil {
			abs = baseURL.ResolveReference(rv).String()
		}
	}
	ep := fixedEndpoint
	if ep == "" {
		// F8: avoid strings.ToLower; check both cases of ".m3u8".
		if strings.Contains(abs, ".m3u8") || strings.Contains(abs, ".M3U8") {
			ep = "/proxy/hls/manifest.m3u8"
		} else {
			ep = "/proxy/stream"
		}
	}
	proxied := h.buildProxyURL(ext, ep, abs, opts)
	return line[:loc[2]] + proxied + line[loc[3]:]
}

// hlsNonHTTPScheme reports whether u carries an explicit scheme other than
// http or https (data:, skd:, urn:, ...), which the proxy cannot fetch.
func hlsNonHTTPScheme(u *url.URL) bool {
	return u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https"
}

// hlsSegmentURLs returns up to max absolute segment URLs (non-playlist URIs)
// from a media playlist, used to warm the cache via prefetch.
func hlsSegmentURLs(base, playlist string, max int) []string {
	if max <= 0 {
		return nil
	}
	// F10: pre-parse base URL once rather than re-parsing per segment line.
	baseURL, baseURLErr := url.Parse(base)
	var urls []string
	for _, line := range strings.Split(playlist, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// F8: avoid strings.ToLower; check both cases of ".m3u8".
		if strings.Contains(line, ".m3u8") || strings.Contains(line, ".M3U8") {
			continue // nested playlist, not a media segment
		}
		// F10: inline resolution against pre-parsed baseURL.
		abs := line
		if rv, err := url.Parse(line); err == nil {
			if hlsNonHTTPScheme(rv) {
				continue // not fetchable through the proxy
			}
			if baseURLErr == nil {
				abs = baseURL.ResolveReference(rv).String()
			}
		}
		urls = append(urls, abs)
		if len(urls) >= max {
			break
		}
	}
	return urls
}
