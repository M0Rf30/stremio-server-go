// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package streamproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/logging"
)

// DVR source resolution (/record). A recording is fed to ffmpeg through this
// server's own /proxy endpoint, so the SSRF guards, request headers, embed
// extraction and the upstream proxy that apply to playback apply to recording
// too. ResolveRecordSource turns whatever the client passed as url= (a media
// URL, an HLS/MPD manifest, an embed page, or an already proxied
// /proxy/... URL) into the proxy path+query that serves it.

// RecordSource describes the URL a client asked to record.
type RecordSource struct {
	URL   string // media URL, embed page, or a /proxy/... URL of this server
	Host  string // optional: force an extractor definition by name
	Proxy string // optional: per-request upstream proxy (socks5/http/https)
}

// RecordError is a resolution failure with the HTTP status to report.
type RecordError struct {
	Status int
	Msg    string
}

func (e *RecordError) Error() string { return e.Msg }

func recErr(status int, format string, a ...any) error {
	return &RecordError{Status: status, Msg: fmt.Sprintf(format, a...)}
}

// IsForbidden reports whether err (from Authorize) is an IP-ACL denial rather
// than a missing/wrong credential.
func IsForbidden(err error) bool { return errors.Is(err, errForbidden) }

// mediaExts are direct media files served by /proxy/stream.
var mediaExts = map[string]bool{
	".mp4": true, ".mkv": true, ".ts": true, ".webm": true, ".avi": true, ".mov": true,
	".flv": true, ".m4v": true, ".mp3": true, ".aac": true, ".m4a": true, ".flac": true, ".ogg": true,
}

const recordSniffTimeout = 10 * time.Second

// ResolveRecordSource returns the path+query (no scheme/host, starting with
// "/proxy/") that serves src with the configured credentials attached. The
// caller prefixes it with a loopback base for ffmpeg, or returns it as the
// relative redirect target a player follows to watch live.
func (h *Handler) ResolveRecordSource(r *http.Request, src RecordSource) (string, error) {
	raw := strings.TrimSpace(src.URL)
	if raw == "" {
		return "", recErr(http.StatusBadRequest, "URL is required")
	}
	dest := decodeDest(raw)
	u, err := url.Parse(dest)
	if err != nil {
		return "", recErr(http.StatusBadRequest, "invalid URL")
	}
	// A /proxy/... or /extractor/... URL of this very server is used as-is
	// (addons hand out such URLs); anything else is wrapped.
	if h.isSelfProxyURL(r, u) {
		q := u.Query()
		if h.cfg.Password != "" && q.Get("api_password") == "" && q.Get("token") == "" {
			q.Set("api_password", h.cfg.Password)
		}
		return u.EscapedPath() + "?" + q.Encode(), nil
	}
	if err := h.ValidateDest(dest); err != nil {
		return "", recErr(http.StatusBadRequest, "%s", err.Error())
	}

	proxyURL := ""
	if p := strings.TrimSpace(src.Proxy); p != "" && p != "on" && p != "off" {
		pu, perr := url.Parse(p)
		if perr != nil {
			return "", recErr(http.StatusBadRequest, "invalid proxy URL")
		}
		switch pu.Scheme {
		case "socks5", "socks5h", "http", "https":
		default:
			return "", recErr(http.StatusBadRequest, "unsupported proxy scheme")
		}
		if err := h.validateProxyHost(p); err != nil {
			return "", recErr(http.StatusForbidden, "proxy not allowed")
		}
		proxyURL = p
	}

	hdr := make(http.Header)
	for k, vs := range r.URL.Query() {
		if after, ok := strings.CutPrefix(k, "h_"); ok && after != "" && !BlockedReqHeader(after) {
			for _, v := range vs {
				hdr.Add(after, v)
			}
		}
	}

	endpoint, final := "", dest
	var ex *Extractor
	name := strings.ToLower(strings.TrimSpace(src.Host))
	if h.defs != nil {
		if name != "" {
			var ok bool
			if ex, ok = h.defs.lookup(name); !ok {
				return "", recErr(http.StatusBadRequest, "unsupported host: %s", src.Host)
			}
		} else if mn, mex, ok := h.defs.match(dest); ok {
			name, ex = mn, mex
		}
	} else if name != "" {
		return "", recErr(http.StatusBadRequest, "unsupported host: %s", src.Host)
	}
	if ex != nil {
		res, err := h.recordExtract(r, name, ex, dest, proxyURL)
		if err != nil {
			return "", err
		}
		endpoint, final = res.Endpoint, res.URL
		for k, v := range res.Headers {
			if hdr.Get(k) == "" {
				hdr.Set(k, v)
			}
		}
	} else {
		endpoint = h.recordEndpoint(r.Context(), u, dest, hdr, proxyURL)
	}
	opts := &Options{ReqHeaders: hdr, RespHeaders: make(http.Header), APIPassword: h.cfg.Password, Proxy: proxyURL}
	return h.buildProxyURL("", endpoint, final, opts), nil
}

// recordExtract runs a definition against the embed page, sharing the
// short-lived resolution cache with the /proxy embed path.
func (h *Handler) recordExtract(r *http.Request, name string, ex *Extractor, page, proxyURL string) (*extractResult, error) {
	key := name + "\x00" + proxyURL + "\x00" + page
	if res, ok := h.embeds.get(key); ok {
		return res, nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), maxDefTimeout)
	defer cancel()
	res, err := ex.run(r.WithContext(ctx), page, h.extractorFetch(proxyURL))
	if err != nil {
		logging.For("extractor").Warn("record: extract failed", "extractor", name, "err", err)
		return nil, recErr(http.StatusBadGateway, "extraction failed: %s", err.Error())
	}
	if err := h.ValidateDest(res.URL); err != nil {
		return nil, recErr(http.StatusBadGateway, "resolved URL rejected: %s", err.Error())
	}
	h.embeds.put(key, res)
	return res, nil
}

// isSelfProxyURL reports whether u points at this server's /proxy or
// /extractor endpoints (by host: the request Host, the public/forwarded base,
// or a loopback address).
func (h *Handler) isSelfProxyURL(r *http.Request, u *url.URL) bool {
	if !strings.HasPrefix(u.Path, "/proxy/") && !strings.HasPrefix(u.Path, "/extractor/") {
		return false
	}
	if u.Host == "" { // relative /proxy/... reference
		return u.Scheme == ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Host)
	bases := []string{r.Host, h.externalBase(r), h.cfg.PublicURL}
	for _, b := range bases {
		if b == "" {
			continue
		}
		if pu, err := url.Parse(b); err == nil && pu.Host != "" {
			b = pu.Host
		}
		if strings.EqualFold(b, host) {
			return true
		}
	}
	switch strings.ToLower(u.Hostname()) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// recordEndpoint picks the /proxy endpoint for a non-extractor destination:
// by file extension when that is conclusive, otherwise by sniffing the first
// bytes through the guarded, upstream-proxy-aware client. An unreadable
// source defaults to HLS (EasyProxy's choice).
func (h *Handler) recordEndpoint(ctx context.Context, u *url.URL, dest string, hdr http.Header, proxyURL string) string {
	const hls, mpd, stream = "/proxy/hls/manifest.m3u8", "/proxy/mpd/manifest.m3u8", "/proxy/stream"
	lp := strings.ToLower(u.Path)
	switch {
	case strings.Contains(lp, ".mpd"):
		return mpd
	case strings.Contains(lp, ".m3u8") || strings.HasSuffix(lp, ".m3u"):
		return hls
	case mediaExts[path.Ext(lp)]:
		return stream
	}
	eff := proxyURL
	if eff == "" {
		eff = h.cfg.UpstreamProxy
	}
	ctx, cancel := context.WithTimeout(ctx, recordSniffTimeout)
	defer cancel()
	sh := hdr.Clone()
	sh.Set("Range", "bytes=0-2047")
	resp, err := h.fetch(ctx, http.MethodGet, dest, sh, nil, eff)
	if err != nil {
		return hls
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return hls
	}
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	head := strings.TrimSpace(string(buf))
	switch {
	case strings.HasPrefix(head, "#EXTM3U"):
		return hls
	case strings.Contains(head, "<MPD"):
		return mpd
	case head == "":
		return hls
	}
	return stream
}
