// Package streamproxy implements an HTTP stream proxy with HLS/DASH manifest
// rewriting, optional segment decryption, signed URLs, and caching.
package streamproxy

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
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

	"golang.org/x/net/proxy"

	"github.com/M0Rf30/stremio-server-go/internal/logging"
)

// Config holds the runtime configuration for the proxy handler.
type Config struct {
	Password      string        // api_password; "" disables password auth
	Secret        []byte        // 32-byte key for signed-URL tokens (AES-GCM)
	IPACL         []*net.IPNet  // client-IP allowlist; empty = allow all
	Prebuffer     int           // upcoming segments to prefetch (0 = off)
	SegCacheTTL   time.Duration // segment cache TTL (0 = caching off)
	PublicURL     string        // explicit external base; "" = derive from request
	Client        *http.Client  // shared streaming HTTP client
	UpstreamProxy string        // global outbound proxy URL; "" = direct (socks5/http/https)
}

// ipCacheEntry holds a resolved public egress IP with an expiry timestamp.
type ipCacheEntry struct {
	ip        string
	expiresAt time.Time
}

// prefetchTimeout is the wall-clock deadline for each prefetch goroutine.
const prefetchTimeout = 30 * time.Second

// maxConcurrentPrefetch caps the total number of prefetch goroutines that may
// run simultaneously across all requests handled by a single Handler.
const maxConcurrentPrefetch = 8

// Handler is the stream proxy request handler.
type Handler struct {
	cfg          Config
	cache        *segCache
	proxyMu      sync.Mutex
	proxyClients map[string]*http.Client
	ipMu         sync.Mutex
	ipCache      map[string]ipCacheEntry
	// prefetchSem is a semaphore that bounds the number of goroutines spawned
	// by prefetch across all concurrent requests.
	prefetchSem chan struct{}
	// signingGCM is the pre-built AES-GCM cipher for token sign/verify (F7).
	// nil when Secret is empty. cipher.AEAD is goroutine-safe.
	signingGCM cipher.AEAD
	// passwordBytes is cfg.Password pre-converted to []byte to avoid a
	// per-request allocation in the constant-time password comparison (F11).
	passwordBytes []byte
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
	// Pre-build the signing GCM once; cipher.AEAD (GCM) is goroutine-safe,
	// so it can be shared across all concurrent requests (F7).
	var gcm cipher.AEAD
	if len(cfg.Secret) > 0 {
		if block, err := aes.NewCipher(cfg.Secret); err == nil {
			gcm, _ = cipher.NewGCM(block)
		}
	}
	if gcm == nil && len(cfg.Secret) > 0 {
		// Secret was set but is not a valid AES key length (16/24/32 bytes);
		// leaving gcm nil here would let signToken/verifyToken's cheap
		// len(h.cfg.Secret)==0 guard pass through to a nil-interface panic.
		// Clearing Secret makes that guard fire instead, degrading cleanly
		// to "signing disabled" rather than crashing every token operation.
		logging.For("streamproxy").Warn("invalid signing secret length; disabling token signing", "len", len(cfg.Secret))
		cfg.Secret = nil
	}
	return &Handler{
		cfg:          cfg,
		cache:        c,
		proxyClients: make(map[string]*http.Client),
		ipCache:      make(map[string]ipCacheEntry),
		prefetchSem:  make(chan struct{}, maxConcurrentPrefetch),
		signingGCM:   gcm,
		// Pre-convert password bytes once to avoid per-request allocation (F11).
		passwordBytes: []byte(cfg.Password),
	}
}

// Options carries decoded proxy request parameters.
type Options struct {
	Dest        string
	ReqHeaders  http.Header
	RespHeaders http.Header
	APIPassword string
	Proxy       string // per-request upstream proxy override (socks5/http/https)
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
	case "ip":
		h.serveIP(w, r)
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

	// Validate proxy param: accept only socks5/socks5h/http/https schemes.
	var proxyParam string
	if raw := q.Get("proxy"); raw != "" {
		if u, err := url.Parse(raw); err == nil {
			switch u.Scheme {
			case "socks5", "socks5h", "http", "https":
				proxyParam = raw
			}
		}
	}

	opts := &Options{
		Dest:        dest,
		ReqHeaders:  make(http.Header),
		RespHeaders: make(http.Header),
		APIPassword: q.Get("api_password"),
		Proxy:       proxyParam,
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

// fetch performs an HTTP request via the given upstream proxy (or the default
// client when proxyURL is ""). Caller is responsible for closing the response Body.
func (h *Handler) fetch(ctx context.Context, method, rawurl string, hdr http.Header, body io.Reader, proxyURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawurl, body)
	if err != nil {
		return nil, err
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return h.clientFor(proxyURL).Do(req)
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
	// Use strings.Builder to avoid O(N) reallocation ladder when appending
	// header parameters in the loop below (F3).
	var b strings.Builder
	b.Grow(256)
	b.WriteString(extBase)
	b.WriteString(endpoint)
	b.WriteString("?d=")
	b.WriteString(base64.RawURLEncoding.EncodeToString([]byte(dest)))
	if opts != nil {
		for k, vs := range opts.ReqHeaders {
			for _, v := range vs {
				b.WriteString("&h_")
				b.WriteString(url.QueryEscape(k))
				b.WriteByte('=')
				b.WriteString(url.QueryEscape(v))
			}
		}
		for k, vs := range opts.RespHeaders {
			for _, v := range vs {
				b.WriteString("&r_")
				b.WriteString(url.QueryEscape(k))
				b.WriteByte('=')
				b.WriteString(url.QueryEscape(v))
			}
		}
		if opts.APIPassword != "" {
			b.WriteString("&api_password=")
			b.WriteString(url.QueryEscape(opts.APIPassword))
		}
		if opts.Proxy != "" {
			b.WriteString("&proxy=")
			b.WriteString(url.QueryEscape(opts.Proxy))
		}
	}
	return b.String()
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

	// Effective upstream proxy: per-request override takes priority over global config.
	effProxy := opts.Proxy
	if effProxy == "" {
		effProxy = h.cfg.UpstreamProxy
	}

	// Decrypt path: fetch the full segment without a Range header, decrypt in
	// memory, and respond 200 with the complete plaintext body.
	if params.Method != "" && len(params.Key) > 0 {
		if segmentDecryptor != nil {
			resp, fetchErr := h.fetch(ctx, http.MethodGet, opts.Dest, upHdr, nil, effProxy)
			if fetchErr != nil {
				http.Error(w, "upstream error", http.StatusBadGateway)
				return
			}
			defer func() { _ = resp.Body.Close() }()
			raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxSegmentBytes+1))
			if readErr != nil {
				http.Error(w, "upstream read error", http.StatusBadGateway)
				return
			}
			if int64(len(raw)) > maxSegmentBytes {
				http.Error(w, "upstream segment too large", http.StatusBadGateway)
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
		data, respHdr, status, cErr := h.cachedFetch(ctx, opts.Dest, upHdr, effProxy)
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
	resp, fetchErr := h.fetch(ctx, r.Method, opts.Dest, upHdr, nil, effProxy)
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
	protected := h.cfg.Password != "" || len(h.cfg.IPACL) > 0 || len(h.cfg.Secret) > 0

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

// ---------------------------------------------------------------------------
// Upstream proxy client management
// ---------------------------------------------------------------------------

// clientFor returns an *http.Client whose transport routes outbound connections
// through proxyURL. When proxyURL is "" the configured base client is returned
// directly. Built clients are cached in h.proxyClients; on build error the base
// client is returned and the error is logged once.
func (h *Handler) clientFor(proxyURL string) *http.Client {
	base := h.cfg.Client
	if base == nil {
		base = http.DefaultClient
	}
	if proxyURL == "" {
		return base
	}
	h.proxyMu.Lock()
	defer h.proxyMu.Unlock()
	if c, ok := h.proxyClients[proxyURL]; ok {
		return c
	}
	c, err := buildProxyClient(proxyURL)
	if err != nil {
		logging.For("streamproxy").Warn("cannot build proxy client; using default", "proxy_url", proxyURL, "err", err)
		return base
	}
	h.proxyClients[proxyURL] = c
	return c
}

// buildProxyClient constructs an *http.Client whose transport routes through
// the given proxy URL (socks5/socks5h/http/https).
func buildProxyClient(proxyURL string) (*http.Client, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy URL: %w", err)
	}
	tr := &http.Transport{
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
	switch u.Scheme {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pw}
		}
		dialer, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("SOCKS5 dialer: %w", err)
		}
		cd, ok := dialer.(proxy.ContextDialer)
		if !ok {
			return nil, fmt.Errorf("SOCKS5 dialer does not implement ContextDialer")
		}
		tr.DialContext = cd.DialContext
	case "http", "https":
		tr.Proxy = http.ProxyURL(u)
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
	return &http.Client{Transport: tr}, nil
}

// ---------------------------------------------------------------------------
// /proxy/ip — egress IP discovery
// ---------------------------------------------------------------------------

// ipCacheTTL is how long a resolved egress IP is cached per effective proxy.
const ipCacheTTL = 5 * time.Minute

// ipCacheMaxEntries is the hard cap on the number of distinct proxy keys
// held in ipCache. Each client-supplied "proxy" query value gets its own
// entry, so without a cap this map would grow unboundedly.
const ipCacheMaxEntries = 16

// ipServices is an ordered list of plain-text / JSON IP-echo endpoints.
var ipServices = []string{
	"https://api.ipify.org",
	"https://checkip.amazonaws.com",
}

// serveIP handles GET /proxy/ip — returns the public egress IP as JSON.
// The effective proxy (opts.Proxy or cfg.UpstreamProxy) is used to fetch.
func (h *Handler) serveIP(w http.ResponseWriter, r *http.Request) {
	if err := h.authorize(r); err != nil {
		writeAuthError(w, err)
		return
	}

	// Determine effective proxy from the request query (same validation as parseOptions).
	effProxy := ""
	if raw := r.URL.Query().Get("proxy"); raw != "" {
		if u, err := url.Parse(raw); err == nil {
			switch u.Scheme {
			case "socks5", "socks5h", "http", "https":
				effProxy = raw
			}
		}
	}
	if effProxy == "" {
		effProxy = h.cfg.UpstreamProxy
	}

	// Serve from cache when still fresh.
	h.ipMu.Lock()
	if e, ok := h.ipCache[effProxy]; ok && time.Now().Before(e.expiresAt) {
		h.ipMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"ip": e.ip})
		return
	}
	h.ipMu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	client := h.clientFor(effProxy)

	var ipStr string
	for _, svcURL := range ipServices {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, svcURL, nil)
		if reqErr != nil {
			continue
		}
		resp, doErr := client.Do(req)
		if doErr != nil {
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64))
		resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK {
			continue
		}
		candidate := strings.TrimSpace(string(body))
		// Try JSON {"ip":"..."} before treating the body as plain text.
		var j struct {
			IP string `json:"ip"`
		}
		if json.Unmarshal(body, &j) == nil && j.IP != "" {
			candidate = j.IP
		}
		if ip := net.ParseIP(candidate); ip != nil {
			ipStr = ip.String()
			break
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if ipStr == "" {
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "could not determine egress IP"})
		return
	}

	// Refresh the cache. Distinct proxy URLs are effectively unbounded (each
	// client-supplied "proxy" query value gets its own entry), so unlike
	// proxyClients (bounded by config cardinality) this map can grow without
	// limit if left untended. Insert first, then sweep: a TTL-only pass
	// removes expired entries, and — mirroring media.hlsManager's
	// sweepProbeCache — a hard-cap loop evicts the soonest-expiring entry
	// until back at ipCacheMaxEntries, so the map is always bounded
	// regardless of how many distinct proxy values a client bursts through.
	h.ipMu.Lock()
	h.ipCache[effProxy] = ipCacheEntry{ip: ipStr, expiresAt: time.Now().Add(ipCacheTTL)}
	if len(h.ipCache) > ipCacheMaxEntries {
		h.sweepIPCache()
	}
	h.ipMu.Unlock()

	_ = json.NewEncoder(w).Encode(map[string]string{"ip": ipStr})
}

// sweepIPCache removes expired entries from the egress-IP cache and, when
// the size still exceeds ipCacheMaxEntries after the TTL sweep, evicts the
// soonest-expiring entry until back under the limit. Must be called with
// h.ipMu held.
func (h *Handler) sweepIPCache() {
	now := time.Now()
	for k, e := range h.ipCache {
		if now.After(e.expiresAt) {
			delete(h.ipCache, k)
		}
	}
	// Hard size cap: evict soonest-expiring entry until under limit.
	for len(h.ipCache) > ipCacheMaxEntries {
		var evict string
		var evictExp time.Time
		found := false
		for k, e := range h.ipCache {
			if !found || e.expiresAt.Before(evictExp) {
				evict = k
				evictExp = e.expiresAt
				found = true
			}
		}
		delete(h.ipCache, evict)
	}
}
