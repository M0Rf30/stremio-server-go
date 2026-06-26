// Package streamproxy implements an HTTP stream proxy with HLS/DASH manifest
// rewriting, optional segment decryption, signed URLs, and caching.
package streamproxy

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config holds the runtime configuration for the proxy handler.
type Config struct {
	Password    string        // api_password; "" disables password auth
	Secret      []byte        // 32-byte key for signed-URL tokens (AES-GCM)
	IPACL       []*net.IPNet  // client-IP allowlist; empty = allow all
	Prebuffer   int           // upcoming segments to prefetch (0 = off)
	SegCacheTTL time.Duration // segment cache TTL (0 = caching off)
	PublicURL   string        // explicit external base; "" = derive from request
	Client      *http.Client  // shared streaming HTTP client
}

// Handler is the stream proxy request handler.
type Handler struct {
	cfg   Config
	cache *segCache
}

// New creates a Handler. A nil Client is replaced with http.DefaultClient.
func New(cfg Config) *Handler {
	if cfg.Client == nil {
		cfg.Client = http.DefaultClient
	}
	var c *segCache
	if cfg.SegCacheTTL > 0 {
		c = newSegCache(cfg.SegCacheTTL, 256)
	}
	return &Handler{cfg: cfg, cache: c}
}

// Options carries decoded proxy request parameters.
type Options struct {
	Dest        string
	ReqHeaders  http.Header
	RespHeaders http.Header
	APIPassword string
}

// DecryptParams carries segment decryption parameters.
type DecryptParams struct {
	Method string
	Key    []byte
	KeyID  []byte
	IV     []byte
}

// Registration hooks set by feature files in init(); foundation compiles with them nil.
var hlsHandler func(h *Handler, w http.ResponseWriter, r *http.Request)
var mpdHandler func(h *Handler, w http.ResponseWriter, r *http.Request)
var segmentDecryptor func(h *Handler, p DecryptParams, segment []byte) ([]byte, error)

// Route dispatches /proxy/* sub-paths. seg[0] is "proxy". Returns true if handled.
func (h *Handler) Route(w http.ResponseWriter, r *http.Request, seg []string) bool {
	if len(seg) < 2 {
		return false
	}
	switch seg[1] {
	case "stream":
		h.serveStream(w, r)
		return true
	case "hls":
		if hlsHandler != nil {
			hlsHandler(h, w, r)
		} else {
			http.Error(w, "HLS proxy not implemented", http.StatusNotImplemented)
		}
		return true
	case "mpd":
		if mpdHandler != nil {
			mpdHandler(h, w, r)
		} else {
			http.Error(w, "MPD proxy not implemented", http.StatusNotImplemented)
		}
		return true
	default:
		return false
	}
}

// HandleGenerateURL handles POST /generate_url and returns a signed token URL.
func (h *Handler) HandleGenerateURL(w http.ResponseWriter, r *http.Request) {
	if err := h.authorize(r); err != nil {
		writeAuthError(w, err)
		return
	}
	if len(h.cfg.Secret) == 0 {
		http.Error(w, "token signing not configured", http.StatusBadRequest)
		return
	}
	var req struct {
		Endpoint      string            `json:"endpoint"`
		Params        map[string]string `json:"params"`
		ExpirySeconds int               `json:"expiry_seconds"`
		IP            string            `json:"ip"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	exp := time.Now().Add(time.Duration(req.ExpirySeconds) * time.Second).Unix()
	tok := token{
		Endpoint: req.Endpoint,
		Params:   req.Params,
		Exp:      exp,
		IP:       req.IP,
	}
	signed, err := h.signToken(tok)
	if err != nil {
		http.Error(w, "failed to sign token", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"url":        req.Endpoint + "?token=" + signed,
		"expires_at": exp,
	})
}

// HandleBase64 handles /base64/encode and /base64/check.
func (h *Handler) HandleBase64(w http.ResponseWriter, r *http.Request, seg []string) {
	if len(seg) < 2 {
		http.NotFound(w, r)
		return
	}
	d := r.URL.Query().Get("d")
	w.Header().Set("Content-Type", "application/json")
	switch seg[1] {
	case "encode":
		enc := base64.RawURLEncoding.EncodeToString([]byte(d))
		_ = json.NewEncoder(w).Encode(map[string]string{"encoded_url": enc})
	case "check":
		decoded, err := tryBase64Decode(d)
		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{"is_base64": false, "decoded": d})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"is_base64": true, "decoded": decoded})
		}
	default:
		http.NotFound(w, r)
	}
}

// parseOptions decodes proxy request parameters from the query string.
// '+' in d is replaced with space before URL/base64 detection.
func (h *Handler) parseOptions(r *http.Request) (*Options, error) {
	q := r.URL.Query()
	raw := strings.ReplaceAll(q.Get("d"), "+", " ")

	var dest string
	switch {
	case strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://"):
		dest = raw
	case raw != "":
		if b, err := base64.RawURLEncoding.DecodeString(raw); err == nil {
			dest = string(b)
		} else if b, err := base64.StdEncoding.DecodeString(raw); err == nil {
			dest = string(b)
		} else {
			dest = raw // keep as-is; caller validates
		}
	}

	opts := &Options{
		Dest:        dest,
		ReqHeaders:  make(http.Header),
		RespHeaders: make(http.Header),
		APIPassword: q.Get("api_password"),
	}
	for k, vs := range q {
		if after, ok := strings.CutPrefix(k, "h_"); ok {
			for _, v := range vs {
				opts.ReqHeaders.Add(after, v)
			}
		} else if after, ok := strings.CutPrefix(k, "r_"); ok {
			for _, v := range vs {
				opts.RespHeaders.Add(after, v)
			}
		}
	}
	return opts, nil
}

// parseDecryptParams reads AES decryption parameters from the query string.
// key, key_id, and iv are accepted as hex or base64 (url or std).
func (h *Handler) parseDecryptParams(r *http.Request) (DecryptParams, error) {
	q := r.URL.Query()
	return DecryptParams{
		Method: q.Get("method"),
		Key:    decodeKeyParam(q.Get("key")),
		KeyID:  decodeKeyParam(q.Get("key_id")),
		IV:     decodeKeyParam(q.Get("iv")),
	}, nil
}

// decodeKeyParam decodes a hex- or base64-encoded key/iv parameter.
func decodeKeyParam(s string) []byte {
	if s == "" {
		return nil
	}
	if b, err := hex.DecodeString(strings.TrimSpace(s)); err == nil {
		return b
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b
	}
	return nil
}

// fetch performs an HTTP request using the configured client. Caller closes Body.
func (h *Handler) fetch(ctx context.Context, method, rawurl string, hdr http.Header, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawurl, body)
	if err != nil {
		return nil, err
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return h.cfg.Client.Do(req)
}

// externalBase returns the external base URL (no trailing slash).
// Uses cfg.PublicURL if set; otherwise derives from X-Forwarded-Proto/Host or r.Host.
func (h *Handler) externalBase(r *http.Request) string {
	if h.cfg.PublicURL != "" {
		return strings.TrimRight(h.cfg.PublicURL, "/")
	}
	scheme := "http"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	host := r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		host = fh
	}
	return scheme + "://" + host
}

// buildProxyURL constructs a proxy URL for the given destination.
// Format: <extBase><endpoint>?d=<base64url(dest)>[&h_/r_ headers][&api_password].
func (h *Handler) buildProxyURL(extBase, endpoint, dest string, opts *Options) string {
	encoded := base64.RawURLEncoding.EncodeToString([]byte(dest))
	u := extBase + endpoint + "?d=" + encoded
	if opts != nil {
		for k, vs := range opts.ReqHeaders {
			for _, v := range vs {
				u += "&h_" + url.QueryEscape(k) + "=" + url.QueryEscape(v)
			}
		}
		for k, vs := range opts.RespHeaders {
			for _, v := range vs {
				u += "&r_" + url.QueryEscape(k) + "=" + url.QueryEscape(v)
			}
		}
		if opts.APIPassword != "" {
			u += "&api_password=" + url.QueryEscape(opts.APIPassword)
		}
	}
	return u
}

// clientIP returns the effective client IP.
// X-Forwarded-For is honoured only when the immediate peer (RemoteAddr) is a
// loopback or private address — i.e., the request is arriving through a local
// reverse proxy. Public clients cannot spoof XFF to bypass IP-ACL checks.
func clientIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)

	// Trust XFF only from a local reverse proxy.
	if peer != nil && isPrivate(peer) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first := strings.SplitN(xff, ",", 2)[0]
			if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
				return ip
			}
		}
	}
	return peer
}

// resolveURL resolves ref against base via net/url.ResolveReference.
func resolveURL(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	rv, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(rv).String()
}

// tryBase64Decode tries base64url then base64std decoding.
func tryBase64Decode(s string) (string, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return string(b), nil
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err == nil {
		return string(b), nil
	}
	return "", err
}

// writeAuthError maps authorize errors to HTTP status codes.
func writeAuthError(w http.ResponseWriter, err error) {
	if errors.Is(err, errForbidden) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// allowedUpstreamHeaders is the forwarding allowlist for upstream response headers.
var allowedUpstreamHeaders = []string{
	"Content-Type",
	"Content-Length",
	"Content-Range",
	"Accept-Ranges",
	"Last-Modified",
	"ETag",
}

// copyAllowedHeaders copies the allowlisted upstream response headers to w.
func copyAllowedHeaders(w http.ResponseWriter, hdr http.Header) {
	for _, k := range allowedUpstreamHeaders {
		if v := hdr.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
}

// applyRespHeaders writes RespHeaders overrides to w, skipping any Access-Control-* key.
func applyRespHeaders(w http.ResponseWriter, rh http.Header) {
	for k, vs := range rh {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") {
			continue
		}
		for _, v := range vs {
			w.Header().Set(k, v)
		}
	}
}

// copyBufPool holds pooled 256 KB buffers for the passthrough streaming copy.
var copyBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 256*1024)
		return &b
	},
}

// serveStream handles GET /proxy/stream — generic stream proxy with optional decryption.
func (h *Handler) serveStream(w http.ResponseWriter, r *http.Request) {
	if err := h.authorize(r); err != nil {
		writeAuthError(w, err)
		return
	}

	opts, err := h.parseOptions(r)
	if err != nil || opts.Dest == "" {
		http.Error(w, "missing or invalid destination URL", http.StatusBadRequest)
		return
	}

	if err := h.ValidateDest(opts.Dest); err != nil {
		http.Error(w, "forbidden destination", http.StatusForbidden)
		return
	}

	params, _ := h.parseDecryptParams(r)

	// Build upstream request headers from opts. Range is forwarded only on the
	// passthrough path; the decrypt path always issues a full GET without Range.
	upHdr := make(http.Header)
	for k, vs := range opts.ReqHeaders {
		upHdr[k] = append([]string(nil), vs...)
	}

	ctx := r.Context()

	// Decrypt path: fetch the full segment without a Range header, decrypt in
	// memory, and respond 200 with the complete plaintext body.
	if params.Method != "" && len(params.Key) > 0 {
		if segmentDecryptor != nil {
			resp, fetchErr := h.fetch(ctx, http.MethodGet, opts.Dest, upHdr, nil)
			if fetchErr != nil {
				http.Error(w, "upstream error", http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			raw, readErr := io.ReadAll(resp.Body)
			if readErr != nil {
				http.Error(w, "upstream read error", http.StatusBadGateway)
				return
			}
			decrypted, decErr := segmentDecryptor(h, params, raw)
			if decErr != nil {
				http.Error(w, "decryption error", http.StatusBadGateway)
				return
			}
			if ct := resp.Header.Get("Content-Type"); ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			applyRespHeaders(w, opts.RespHeaders)
			w.Header().Set("Content-Length", strconv.Itoa(len(decrypted)))
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = w.Write(decrypted)
			}
			return
		}
		// segmentDecryptor nil — fall through to passthrough.
	}

	// Passthrough path: forward the client Range header upstream.
	if rng := r.Header.Get("Range"); rng != "" {
		upHdr.Set("Range", rng)
	}

	// Non-Range GET: use the segment cache when TTL is configured.
	if r.Method == http.MethodGet && r.Header.Get("Range") == "" && h.cfg.SegCacheTTL > 0 {
		data, respHdr, status, cErr := h.cachedFetch(ctx, opts.Dest, upHdr)
		if cErr != nil {
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		copyAllowedHeaders(w, respHdr)
		applyRespHeaders(w, opts.RespHeaders)
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			_, _ = w.Write(data)
		}
		return
	}

	// Direct streaming path (Range requests, HEAD, or caching off).
	resp, fetchErr := h.fetch(ctx, r.Method, opts.Dest, upHdr, nil)
	if fetchErr != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyAllowedHeaders(w, resp.Header)
	applyRespHeaders(w, opts.RespHeaders)
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		bufp := copyBufPool.Get().(*[]byte)
		_, _ = io.CopyBuffer(w, resp.Body, *bufp)
		copyBufPool.Put(bufp)
	}
}

// ---------------------------------------------------------------------------
// SSRF guard helpers
// ---------------------------------------------------------------------------

// privateRanges lists IP ranges that are considered private/non-routable.
var privateRanges []*net.IPNet

func init() {
	for _, cidr := range []string{
		"127.0.0.0/8",    // IPv4 loopback
		"10.0.0.0/8",     // RFC 1918
		"172.16.0.0/12",  // RFC 1918
		"192.168.0.0/16", // RFC 1918
		"169.254.0.0/16", // IPv4 link-local
		"::1/128",        // IPv6 loopback
		"fc00::/7",       // IPv6 ULA
		"fe80::/10",      // IPv6 link-local
	} {
		_, network, _ := net.ParseCIDR(cidr)
		if network != nil {
			privateRanges = append(privateRanges, network)
		}
	}
}

// isPrivate reports whether ip is a loopback, RFC 1918, link-local, or ULA address.
// IPv4-mapped IPv6 addresses are normalised to IPv4 before matching.
func isPrivate(ip net.IP) bool {
	// Normalise so IPv4-mapped IPv6 (::ffff:x.x.x.x) matches v4 ranges.
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	for _, r := range privateRanges {
		if r.Contains(ip) {
			return true
		}
	}
	return false
}

// isCloudMetadata reports whether ip is the well-known cloud metadata address
// 169.254.169.254 (or its IPv4-mapped IPv6 form ::ffff:169.254.169.254).
func isCloudMetadata(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	return ip4[0] == 169 && ip4[1] == 254 && ip4[2] == 169 && ip4[3] == 254
}

// ValidateDest is an SSRF guard for proxy destination URLs.
// It rejects non-http(s) schemes, resolves the hostname, and blocks:
//   - 169.254.169.254 (cloud-metadata endpoint) — always.
//   - Loopback, RFC 1918, link-local, and ULA addresses — only when the proxy
//     has a password or IP-ACL configured (i.e., it is exposed to untrusted clients).
//
// Returns nil when the destination is permitted.
func (h *Handler) ValidateDest(rawurl string) error {
	u, err := url.Parse(rawurl)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("disallowed scheme %q: only http and https are permitted", u.Scheme)
	}

	host := u.Hostname()

	// Collect IPs to check: direct parse for numeric hosts, DNS lookup otherwise.
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		addrs, lookupErr := net.LookupHost(host)
		if lookupErr != nil {
			return fmt.Errorf("cannot resolve %q: %w", host, lookupErr)
		}
		for _, a := range addrs {
			if ip := net.ParseIP(a); ip != nil {
				ips = append(ips, ip)
			}
		}
	}

	// The proxy is "protected" when any auth mechanism is active.
	protected := h.cfg.Password != "" || len(h.cfg.IPACL) > 0

	for _, ip := range ips {
		if isCloudMetadata(ip) {
			return fmt.Errorf("destination resolves to disallowed cloud-metadata address %s", ip)
		}
		if protected && isPrivate(ip) {
			return fmt.Errorf("destination resolves to private address %s (proxy is protected)", ip)
		}
	}
	return nil
}

// Authorize is the exported wrapper over the internal authorize method.
// It is consumed by the api package to gate requests before routing.
// Returns the same sentinel errors as authorize (errUnauthorized, errForbidden).
func (h *Handler) Authorize(r *http.Request) error {
	return h.authorize(r)
}
