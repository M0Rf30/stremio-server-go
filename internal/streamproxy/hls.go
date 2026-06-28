package streamproxy

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// hlsURIAttrRe matches the URI="..." attribute within an HLS tag line.
var hlsURIAttrRe = regexp.MustCompile(`URI="([^"]*)"`)

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
	effProxy := opts.Proxy
	if effProxy == "" {
		effProxy = h.cfg.UpstreamProxy
	}
	resp, err := h.fetch(r.Context(), "GET", opts.Dest, opts.ReqHeaders, nil, effProxy)
	if err != nil {
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		http.Error(w, "reading upstream response failed", http.StatusBadGateway)
		return
	}
	if len(body) > maxManifestBytes {
		http.Error(w, "upstream manifest too large", http.StatusBadGateway)
		return
	}
	if h.cfg.Prebuffer > 0 {
		h.prefetch(context.WithoutCancel(r.Context()), hlsSegmentURLs(opts.Dest, string(body), h.cfg.Prebuffer), opts.ReqHeaders, effProxy)
	}
	rewritten := hlsRewrite(h, r, opts, string(body))
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
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
//   - #EXT-X-KEY and #EXT-X-MAP URI="..." attributes are always routed to
//     /proxy/stream; all other attributes (METHOD, IV, BYTERANGE, …) are kept.
//   - #EXT-X-MEDIA and #EXT-X-I-FRAME-STREAM-INF URI="..." attributes are
//     routed to /proxy/hls/manifest.m3u8 when the URL contains ".m3u8",
//     otherwise to /proxy/stream.
func hlsRewrite(h *Handler, r *http.Request, opts *Options, playlist string) string {
	lines := strings.Split(playlist, "\n")
	ext := h.externalBase(r)
	base := opts.Dest

	out := make([]string, 0, len(lines))
	nextIsVariant := false // true after #EXT-X-STREAM-INF until next URI line

	for _, line := range lines {
		if line == "" {
			out = append(out, line)
			continue
		}

		if !strings.HasPrefix(line, "#") {
			// Bare URI line.
			abs := resolveURL(base, line)
			ep := "/proxy/stream"
			if nextIsVariant || strings.Contains(strings.ToLower(abs), ".m3u8") {
				ep = "/proxy/hls/manifest.m3u8"
			}
			nextIsVariant = false
			out = append(out, h.buildProxyURL(ext, ep, abs, opts))
			continue
		}

		// Tag or comment line.
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "#EXT-X-STREAM-INF:"):
			// URI follows on the next non-comment line.
			nextIsVariant = true
			out = append(out, line)

		case strings.HasPrefix(upper, "#EXT-X-KEY") || strings.HasPrefix(upper, "#EXT-X-MAP"):
			// Encryption key or initialization segment: always /proxy/stream.
			out = append(out, hlsRewriteURIAttr(line, base, ext, opts, h, "/proxy/stream"))

		case strings.HasPrefix(upper, "#EXT-X-MEDIA") || strings.HasPrefix(upper, "#EXT-X-I-FRAME-STREAM-INF"):
			// Alternate rendition or I-frame playlist: endpoint depends on URL.
			out = append(out, hlsRewriteURIAttr(line, base, ext, opts, h, ""))

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
func hlsRewriteURIAttr(line, base, ext string, opts *Options, h *Handler, fixedEndpoint string) string {
	sub := hlsURIAttrRe.FindStringSubmatch(line)
	if sub == nil {
		return line
	}
	ref := sub[1]
	abs := resolveURL(base, ref)
	ep := fixedEndpoint
	if ep == "" {
		if strings.Contains(strings.ToLower(abs), ".m3u8") {
			ep = "/proxy/hls/manifest.m3u8"
		} else {
			ep = "/proxy/stream"
		}
	}
	proxied := h.buildProxyURL(ext, ep, abs, opts)
	return strings.Replace(line, `URI="`+ref+`"`, `URI="`+proxied+`"`, 1)
}

// hlsSegmentURLs returns up to max absolute segment URLs (non-playlist URIs)
// from a media playlist, used to warm the cache via prefetch.
func hlsSegmentURLs(base, playlist string, max int) []string {
	if max <= 0 {
		return nil
	}
	var urls []string
	for _, line := range strings.Split(playlist, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(strings.ToLower(line), ".m3u8") {
			continue // nested playlist, not a media segment
		}
		urls = append(urls, resolveURL(base, line))
		if len(urls) >= max {
			break
		}
	}
	return urls
}
