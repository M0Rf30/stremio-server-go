// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package streamproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/M0Rf30/stremio-server-go/internal/logging"
)

// MediaFlow-compatible /extractor/video endpoint: resolve a hoster page
// (host=…, d=<page url>) into a playable URL and either redirect to the
// matching /proxy/* endpoint (redirect_stream=true) or return MediaFlow's
// JSON description of it (redirect_stream=false).

// maxExtractorPage caps how much of an upstream hoster page is read.
const maxExtractorPage = 4 << 20

// extractorUA is sent to hosters; several refuse non-browser user agents.
const extractorUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"

// errExtract marks a page that was fetched but could not be resolved.
var errExtract = errors.New("extraction failed")

// extractResult is a resolved stream plus the headers needed to fetch it.
type extractResult struct {
	URL      string
	Headers  map[string]string
	Endpoint string // /proxy/stream or /proxy/hls/manifest.m3u8
}

// pageFetcher fetches a page with the given headers and returns its body.
type pageFetcher func(r *http.Request, rawurl string, hdr map[string]string) (string, error)

type extractorFunc func(r *http.Request, page string, get pageFetcher) (*extractResult, error)

// extractors maps the lowercased MediaFlow host name to its resolver.
var extractors = map[string]extractorFunc{
	"vixcloud": extractVixCloud,
}

// mediaflowEndpointName maps proxy paths to MediaFlow's endpoint identifiers.
var mediaflowEndpointName = map[string]string{
	"/proxy/stream":            "proxy_stream_endpoint",
	"/proxy/hls/manifest.m3u8": "hls_manifest_proxy",
}

// HandleExtractor handles GET /extractor/video[.m3u8|.mp4].
//
// @Summary  Resolve a hoster page into a proxied stream (MediaFlow-compatible)
// @Tags     Proxy
// @Param    host             query string true  "hoster: VixCloud"
// @Param    d                query string true  "page URL (plain or base64)"
// @Param    redirect_stream  query bool   false "302 to the proxy URL instead of JSON"
// @Success  200
// @Success  302
// @Failure  400
// @Failure  401
// @Failure  502
// @Router   /extractor/video [get]
func (h *Handler) HandleExtractor(w http.ResponseWriter, r *http.Request, seg []string) {
	if len(seg) < 2 || strings.TrimSuffix(strings.TrimSuffix(seg[1], ".m3u8"), ".mp4") != "video" {
		http.NotFound(w, r)
		return
	}
	if err := h.authorize(r); err != nil {
		writeAuthError(w, err)
		return
	}
	q := r.URL.Query()
	hostName := strings.ToLower(strings.TrimSpace(q.Get("host")))
	fn, ok := extractors[hostName]
	if !ok {
		writeExtractorError(w, http.StatusBadRequest, "unsupported host: "+q.Get("host"))
		return
	}
	page := decodeDest(q.Get("d"))
	if err := h.ValidateDest(page); err != nil {
		writeExtractorError(w, http.StatusBadRequest, err.Error())
		return
	}
	opts, err := h.parseOptions(r)
	if err != nil {
		writeExtractorError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := fn(r, page, h.extractorFetch(opts.Proxy))
	if err != nil {
		logging.For("extractor").Warn("extract failed", "host", hostName, "err", err)
		writeExtractorError(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := h.ValidateDest(res.URL); err != nil {
		writeExtractorError(w, http.StatusBadGateway, "resolved URL rejected: "+err.Error())
		return
	}
	for k, v := range res.Headers {
		opts.ReqHeaders.Set(k, v)
	}
	base := h.externalBase(r)
	if b, _ := strconv.ParseBool(q.Get("redirect_stream")); b {
		http.Redirect(w, r, h.buildProxyURL(base, res.Endpoint, res.URL, opts), http.StatusFound)
		return
	}
	qp := map[string]string{}
	if opts.APIPassword != "" {
		qp["api_password"] = opts.APIPassword
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"destination_url":     res.URL,
		"request_headers":     res.Headers,
		"mediaflow_endpoint":  mediaflowEndpointName[res.Endpoint],
		"mediaflow_proxy_url": base + res.Endpoint,
		"query_params":        qp,
	})
}

func writeExtractorError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"detail": msg})
}

// extractorFetch returns a pageFetcher using the SSRF-guarded proxy client.
func (h *Handler) extractorFetch(proxyURL string) pageFetcher {
	return func(r *http.Request, rawurl string, hdr map[string]string) (string, error) {
		if err := h.ValidateDest(rawurl); err != nil {
			return "", err
		}
		hh := http.Header{"User-Agent": {extractorUA}}
		for k, v := range hdr {
			hh.Set(k, v)
		}
		resp, err := h.fetch(r.Context(), http.MethodGet, rawurl, hh, nil, proxyURL)
		if err != nil {
			return "", err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("%w: upstream status %d", errExtract, resp.StatusCode)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxExtractorPage))
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
}

func origin(rawurl string) string {
	u, err := url.Parse(rawurl)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

var (
	vixTokenRe   = regexp.MustCompile(`['"]token['"]\s*:\s*['"](\w+)['"]`)
	vixExpiresRe = regexp.MustCompile(`['"]expires['"]\s*:\s*['"](\d+)['"]`)
	vixURLRe     = regexp.MustCompile(`url\s*:\s*['"](https?://[^'"]+)['"]`)
	vixFHDRe     = regexp.MustCompile(`window\.canPlayFHD\s*=\s*true`)
	vixIframeRe  = regexp.MustCompile(`<iframe[^>]+src=["']([^"']+)["']`)
	vixAPIPath   = regexp.MustCompile(`^/(movie|tv)/`)
)

// extractVixCloud resolves VixCloud-style embed pages: the page script
// carries window.masterPlaylist {url, params{token, expires}} and
// window.canPlayFHD; the signed master playlist is url?token&expires[&h=1].
// Client-side /movie|tv/ pages first resolve their embed path via
// /api/movie|tv/… ({"src": "/embed/…"}); /iframe pages wrap the real
// player in an <iframe>.
func extractVixCloud(r *http.Request, page string, get pageFetcher) (*extractResult, error) {
	ref := origin(page) + "/"
	target := page
	if u, err := url.Parse(page); err == nil && vixAPIPath.MatchString(u.Path) {
		u.Path = "/api" + u.Path
		if js, err := get(r, u.String(), map[string]string{"Referer": page}); err == nil {
			var api struct {
				Src string `json:"src"`
			}
			if json.Unmarshal([]byte(js), &api) == nil && api.Src != "" {
				target = resolveURL(page, api.Src)
			}
		}
	}
	body, err := get(r, target, map[string]string{"Referer": page})
	if err != nil {
		return nil, err
	}
	if strings.Contains(target, "/iframe") {
		if m := vixIframeRe.FindStringSubmatch(body); m != nil {
			player := resolveURL(target, html.UnescapeString(m[1]))
			if body, err = get(r, player, map[string]string{"Referer": target}); err != nil {
				return nil, err
			}
			ref = origin(player) + "/"
		}
	}
	tok := vixTokenRe.FindStringSubmatch(body)
	exp := vixExpiresRe.FindStringSubmatch(body)
	pl := vixURLRe.FindStringSubmatch(body)
	if tok == nil || exp == nil || pl == nil {
		return nil, fmt.Errorf("%w: masterPlaylist not found", errExtract)
	}
	u, err := url.Parse(pl[1])
	if err != nil {
		return nil, fmt.Errorf("%w: bad playlist url: %w", errExtract, err)
	}
	qv := u.Query()
	nq := url.Values{}
	if qv.Get("b") != "" {
		nq.Set("b", qv.Get("b"))
	}
	nq.Set("token", tok[1])
	nq.Set("expires", exp[1])
	if vixFHDRe.MatchString(body) {
		nq.Set("h", "1")
	}
	u.RawQuery = nq.Encode()
	return &extractResult{
		URL:      u.String(),
		Headers:  map[string]string{"Referer": ref, "Origin": strings.TrimSuffix(ref, "/")},
		Endpoint: "/proxy/hls/manifest.m3u8",
	}, nil
}
