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
//   - Bare URI lines (not starting with '#') are proxied through
//     /proxy/stream/segment.<ext> (see hlsSegmentEndpoint), except when
//     following an #EXT-X-STREAM-INF tag or when the URL contains ".m3u8",
//     in which case /proxy/hls/manifest.m3u8 is used.
//   - #EXT-X-MAP, #EXT-X-PART and #EXT-X-PRELOAD-HINT URI="..." attributes
//     are media segments and also use /proxy/stream/segment.<ext>;
//     #EXT-X-KEY and #EXT-X-SESSION-KEY use plain /proxy/stream. All other
//     attributes (METHOD, IV, BYTERANGE, …) are kept.
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
	// Header/credential query parts are identical for every URL of this
	// playlist: escape them once.
	pb := h.newProxyURLBuilder(opts)
	nextIsVariant := false // true after #EXT-X-STREAM-INF until next URI line
	// Extension-less segments get a label matching their likely container:
	// a playlist with #EXT-X-MAP carries fMP4 segments, otherwise MPEG-TS.
	// ffmpeg rejects segments whose label mismatches the detected format.
	segDefault := "ts"
	if strings.Contains(playlist, "#EXT-X-MAP") {
		segDefault = "mp4"
	}

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
			abs, skip := hlsResolve(baseURL, baseURLErr, line)
			if skip {
				nextIsVariant = false
				out = append(out, line)
				continue
			}
			ep := hlsSegmentEndpoint(abs, segDefault)
			// F8: avoid strings.ToLower; check both cases of ".m3u8".
			if nextIsVariant || strings.Contains(abs, ".m3u8") || strings.Contains(abs, ".M3U8") {
				ep = "/proxy/hls/manifest.m3u8"
			}
			nextIsVariant = false
			out = append(out, pb.build(ext, ep, abs))
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

		case "#EXT-X-KEY", "#EXT-X-SESSION-KEY":
			// Encryption keys are not extension-checked by players.
			out = append(out, hlsRewriteURIAttr(line, baseURL, ext, pb, "/proxy/stream"))

		case "#EXT-X-MAP":
			// Initialization segment (fMP4): give it a media extension.
			out = append(out, hlsRewriteURIAttrFunc(line, baseURL, ext, pb, func(abs string) string {
				return hlsSegmentEndpoint(abs, "mp4")
			}))

		case "#EXT-X-PART", "#EXT-X-PRELOAD-HINT":
			// LL-HLS part or preload hint: media segments.
			out = append(out, hlsRewriteURIAttrFunc(line, baseURL, ext, pb, func(abs string) string {
				return hlsSegmentEndpoint(abs, segDefault)
			}))

		case "#EXT-X-RENDITION-REPORT":
			// LL-HLS rendition report: always a media playlist.
			out = append(out, hlsRewriteURIAttr(line, baseURL, ext, pb, "/proxy/hls/manifest.m3u8"))

		case "#EXT-X-MEDIA", "#EXT-X-I-FRAME-STREAM-INF":
			// Alternate rendition or I-frame playlist: always a media playlist.
			out = append(out, hlsRewriteURIAttr(line, baseURL, ext, pb, "/proxy/hls/manifest.m3u8"))

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
func hlsRewriteURIAttr(line string, baseURL *url.URL, ext string, pb *proxyURLBuilder, fixedEndpoint string) string {
	return hlsRewriteURIAttrFunc(line, baseURL, ext, pb, func(abs string) string {
		if fixedEndpoint != "" {
			return fixedEndpoint
		}
		// F8: avoid strings.ToLower; check both cases of ".m3u8".
		if strings.Contains(abs, ".m3u8") || strings.Contains(abs, ".M3U8") {
			return "/proxy/hls/manifest.m3u8"
		}
		return "/proxy/stream"
	})
}

// hlsRewriteURIAttrFunc is hlsRewriteURIAttr with the endpoint chosen by
// endpointFor from the resolved absolute URI.
func hlsRewriteURIAttrFunc(line string, baseURL *url.URL, ext string, pb *proxyURLBuilder, endpointFor func(abs string) string) string {
	loc := hlsURIAttrRe.FindStringSubmatchIndex(line)
	if loc == nil {
		return line
	}
	ref := line[loc[2]:loc[3]]
	if ref == "" {
		return line
	}
	abs, skip := hlsResolve(baseURL, nil, ref)
	if skip {
		return line
	}
	proxied := pb.build(ext, endpointFor(abs), abs)
	return line[:loc[2]] + proxied + line[loc[3]:]
}

// hlsSegmentExts are the segment extensions ffmpeg's HLS demuxer accepts by
// default (allowed_segment_extensions; enforced since ffmpeg 7.1 via
// extension_picky). Proxied segment URLs must end in one of them or
// ffmpeg-based players (mpv, Stremio desktop) refuse to load the segment.
var hlsSegmentExts = map[string]bool{
	"3gp": true, "3gpp": true, "aac": true, "ac3": true, "avi": true, "ec3": true,
	"fmp4": true, "m2ts": true, "m4a": true, "m4s": true, "m4v": true, "mkv": true,
	"mov": true, "mp3": true, "mp4": true, "mpeg": true, "mpegts": true, "ogg": true,
	"ts": true, "vob": true, "wav": true,
	// Subtitle renditions: ffmpeg also rejects a segment whose detected
	// format does not match its extension, so WebVTT must stay .vtt.
	"vtt": true, "webvtt": true,
}

// hlsSegmentEndpoints maps each accepted extension to its constant endpoint,
// so choosing an endpoint never allocates.
var hlsSegmentEndpoints = func() map[string]string {
	m := make(map[string]string, len(hlsSegmentExts))
	for e := range hlsSegmentExts {
		m[e] = "/proxy/stream/segment." + e
	}
	return m
}()

// hlsSegmentEndpoint returns /proxy/stream/segment.<ext> for a media segment:
// the upstream path's extension when it is an accepted segment extension,
// otherwise def. The trailing path element is cosmetic; /proxy/stream/*
// serves the same handler as /proxy/stream. It works on the string directly
// (no url.Parse): this runs once per segment of every rewritten playlist.
func hlsSegmentEndpoint(abs, def string) string {
	p := abs
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if i := strings.Index(p, "://"); i >= 0 { // drop scheme://authority
		p = p[i+3:]
		if j := strings.IndexByte(p, '/'); j >= 0 {
			p = p[j:]
		} else {
			p = ""
		}
	}
	base := p[strings.LastIndexByte(p, '/')+1:]
	if i := strings.LastIndexByte(base, '.'); i >= 0 {
		e := base[i+1:]
		if ep, ok := hlsSegmentEndpoints[e]; ok {
			return ep
		}
		if ep, ok := hlsSegmentEndpoints[strings.ToLower(e)]; ok { // rare: .TS, .M4S
			return ep
		}
	}
	if ep, ok := hlsSegmentEndpoints[def]; ok {
		return ep
	}
	return "/proxy/stream/segment." + def
}

// hlsNonHTTPScheme reports whether u carries an explicit scheme other than
// http or https (data:, skd:, urn:, ...), which the proxy cannot fetch.
func hlsNonHTTPScheme(u *url.URL) bool {
	return u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https"
}

// hlsPlainAbsolute reports whether ref is an absolute http(s) URL that
// url.Parse + ResolveReference + String would return unchanged, so the
// per-segment parse can be skipped. It is deliberately conservative: any
// character or shape that URL normalisation could rewrite (dot segments,
// "//" in the path, percent-escapes, userinfo, fragments, spaces, uppercase
// schemes, an empty path) takes the slow path.
func hlsPlainAbsolute(ref string) bool {
	var rest string
	switch {
	case strings.HasPrefix(ref, "https://"):
		rest = ref[len("https://"):]
	case strings.HasPrefix(ref, "http://"):
		rest = ref[len("http://"):]
	default:
		return false
	}
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		return false // no host or no path: ResolveReference may add "/"
	}
	pathEnd := len(rest)
	for i := range len(rest) {
		c := rest[i]
		if c == '%' || c == '#' || c == '@' || c == '\\' || c <= ' ' || c >= 0x7f {
			return false
		}
		if c == '?' && pathEnd == len(rest) {
			pathEnd = i
		}
	}
	if pathEnd < slash {
		return false // "?" before the first "/": the path is empty
	}
	p := rest[slash:pathEnd]
	return !strings.Contains(p, "/.") && !strings.Contains(p, "//")
}

// hlsResolve resolves a playlist URI against the playlist URL exactly like
// url.Parse + baseURL.ResolveReference(...).String(), skipping the parse for
// plain absolute http(s) URIs. skip reports a non-http(s) scheme (data:,
// skd:, ...) that must be left untouched. hlsRewrite and hlsSegmentURLs both
// use it, so prefetch cache keys equal the URLs the player requests.
func hlsResolve(baseURL *url.URL, baseURLErr error, ref string) (abs string, skip bool) {
	if hlsPlainAbsolute(ref) {
		return ref, false
	}
	rv, err := url.Parse(ref)
	if err != nil {
		return ref, false
	}
	if hlsNonHTTPScheme(rv) {
		return ref, true
	}
	if baseURLErr == nil && baseURL != nil {
		return baseURL.ResolveReference(rv).String(), false
	}
	return ref, false
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
		abs, skip := hlsResolve(baseURL, baseURLErr, line)
		if skip {
			continue // not fetchable through the proxy
		}
		urls = append(urls, abs)
		if len(urls) >= max {
			break
		}
	}
	return urls
}
