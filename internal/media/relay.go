package media

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/netguard"
)

// relayTokenTTL is how long a registered relay token remains valid after
// being handed to ffmpeg/ffprobe. Generous enough to outlive a full HLS
// transcode session (see sessionTTL); expired entries are swept by the
// janitor, not evicted on read, since a long-paused player may still come
// back and re-request a segment.
const relayTokenTTL = 2 * time.Hour

// relayJanitorInterval is how often expired relay tokens are swept.
const relayJanitorInterval = 5 * time.Minute

// relayMaxPlaylistBytes caps the size of an HLS playlist the relay will
// rewrite and serve, guarding against a malicious/huge upstream playlist
// exhausting memory. Real playlists are a few KB; 4 MiB is a generous cap.
const relayMaxPlaylistBytes = 4 << 20 // 4 MiB

// relayMaxRedirects is the hard cap on HTTP redirects the relay's own fetch
// will follow on behalf of ffmpeg/ffprobe.
const relayMaxRedirects = 5

// relayURIAttr matches a quoted URI="..." attribute inside an HLS tag line
// (e.g. #EXT-X-KEY, #EXT-X-MEDIA, #EXT-X-MAP).
var relayURIAttr = regexp.MustCompile(`URI="([^"]*)"`)

// hopHeaders are per-connection headers that must never be forwarded verbatim
// between the relay and either side of it.
var hopHeaders = map[string]bool{
	"Connection":          true,
	"Proxy-Connection":    true,
	"Keep-Alive":          true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Te":                  true,
	"Trailer":             true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
}

// relayEntry is one registered upstream URL a relay token maps to.
type relayEntry struct {
	url       string
	expiresAt time.Time
}

// mediaRelay is a loopback-only HTTP proxy that sits between ffmpeg/ffprobe
// and every remote (non-selfBase) media URL (SEC-4).
//
// Without it, this process's pre-flight IP validation (validateRemoteURL) is
// the only SSRF check: ffmpeg/ffprobe are handed the real hostname and do
// their own DNS resolution (open to DNS rebinding), follow HTTP redirects
// themselves, and — for HLS — fetch nested playlist/segment URLs referenced
// inside the playlist body, none of which this process ever sees or
// re-validates.
//
// With the relay, ffmpeg/ffprobe are instead given a
// "http://127.0.0.1:<port>/r/<token>" URL. This process resolves the token,
// fetches the real upstream URL itself through client (whose dialer Control
// hook re-validates the actually-resolved IP at connect time — the same
// netguard.DialControl pattern used by openSubClient), and streams the
// response back. Nested HLS playlist URIs are rewritten to fresh relay
// tokens before the playlist is handed to ffmpeg, so every hop of a
// multi-level HLS fetch is funneled back through the same guarded client.
type mediaRelay struct {
	client *http.Client

	startOnce sync.Once
	startErr  error
	ln        net.Listener
	srv       *http.Server

	mu     sync.Mutex
	tokens map[string]relayEntry
}

// globalRelay is the process-wide relay instance used by every ffmpeg/
// ffprobe call site in this package. It blocks private/loopback/
// cloud-metadata addresses (blockPrivate=true) — the correct posture for
// externally-supplied media URLs.
var globalRelay = newMediaRelay(true)

// newMediaRelay constructs a relay whose outbound fetches are guarded by
// netguard.DialControl(blockPrivate). Tests that need to point the relay at
// an httptest.Server (necessarily loopback) construct their own instance
// with blockPrivate=false instead of mutating globalRelay.
func newMediaRelay(blockPrivate bool) *mediaRelay {
	return &mediaRelay{
		tokens: make(map[string]relayEntry),
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout: 10 * time.Second,
					Control: netguard.DialControl(blockPrivate),
				}).DialContext,
			},
			CheckRedirect: func(_ *http.Request, via []*http.Request) error {
				if len(via) >= relayMaxRedirects {
					return fmt.Errorf("media relay: stopped after %d redirects", relayMaxRedirects)
				}
				// No separate host check here: the dialer's Control hook
				// re-validates the resolved IP of every redirect target at
				// connect time, exactly as it does for the initial request.
				return nil
			},
		},
	}
}

// start lazily binds the loopback listener and HTTP server, the first time
// it's needed. Safe to call repeatedly and concurrently.
func (r *mediaRelay) start() error {
	r.startOnce.Do(func() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			r.startErr = fmt.Errorf("media relay: listen: %w", err)
			return
		}
		r.ln = ln
		mux := http.NewServeMux()
		mux.HandleFunc("/r/", r.handle)
		r.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = r.srv.Serve(ln) }()
		go r.janitor()
	})
	return r.startErr
}

// port returns the loopback port the relay is listening on. Must be called
// after a successful start().
func (r *mediaRelay) port() int {
	return r.ln.Addr().(*net.TCPAddr).Port
}

// register starts the relay if needed and returns a fresh
// "http://127.0.0.1:<port>/r/<token>" URL that proxies to rawURL.
func (r *mediaRelay) register(rawURL string) (string, error) {
	if err := r.start(); err != nil {
		return "", err
	}
	token := randomRelayToken()
	r.mu.Lock()
	r.tokens[token] = relayEntry{url: rawURL, expiresAt: time.Now().Add(relayTokenTTL)}
	r.mu.Unlock()
	return fmt.Sprintf("http://127.0.0.1:%d/r/%s", r.port(), token), nil
}

func randomRelayToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// resolve looks up the upstream URL for token, rejecting unknown or expired
// tokens.
func (r *mediaRelay) resolve(token string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.tokens[token]
	if !ok || time.Now().After(e.expiresAt) {
		return "", false
	}
	return e.url, true
}

// janitor periodically sweeps expired tokens so long-running processes don't
// accumulate an unbounded token map across many playback sessions.
func (r *mediaRelay) janitor() {
	ticker := time.NewTicker(relayJanitorInterval)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		r.mu.Lock()
		for k, v := range r.tokens {
			if now.After(v.expiresAt) {
				delete(r.tokens, k)
			}
		}
		r.mu.Unlock()
	}
}

// handle serves GET/HEAD requests under /r/<token>, proxying to the
// registered upstream URL.
func (r *mediaRelay) handle(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimPrefix(req.URL.Path, "/r/")
	if idx := strings.IndexByte(token, '/'); idx >= 0 {
		token = token[:idx]
	}
	upstream, ok := r.resolve(token)
	if !ok {
		http.Error(w, "unknown or expired relay token", http.StatusNotFound)
		return
	}
	r.proxy(w, req, upstream)
}

// proxy fetches upstream through r.client (forwarding Range and the
// method), rewriting nested URIs if the response looks like an HLS
// playlist, and otherwise streaming status/headers/body straight through.
func (r *mediaRelay) proxy(w http.ResponseWriter, req *http.Request, upstream string) {
	outReq, err := http.NewRequestWithContext(req.Context(), req.Method, upstream, nil)
	if err != nil {
		http.Error(w, "bad upstream url", http.StatusBadGateway)
		return
	}
	if rng := req.Header.Get("Range"); rng != "" {
		outReq.Header.Set("Range", rng)
	}
	resp, err := r.client.Do(outReq)
	if err != nil {
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if req.Method == http.MethodGet && looksLikePlaylist(resp, upstream) {
		r.servePlaylist(w, resp, upstream)
		return
	}

	copyProxyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if req.Method != http.MethodHead {
		_, _ = io.Copy(w, resp.Body)
	}
}

// looksLikePlaylist reports whether resp is (very likely) an HLS playlist,
// based on Content-Type or the upstream URL's extension — checked without
// reading the body, so ordinary media segments are streamed straight through
// instead of being buffered.
func looksLikePlaylist(resp *http.Response, upstream string) bool {
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "mpegurl") {
		return true
	}
	if u, err := url.Parse(upstream); err == nil && strings.HasSuffix(strings.ToLower(u.Path), ".m3u8") {
		return true
	}
	return false
}

// servePlaylist reads resp's body (capped at relayMaxPlaylistBytes), rewrites
// every referenced URI (segment or nested playlist) to a fresh relay token
// resolved against upstream, and serves the rewritten playlist.
func (r *mediaRelay) servePlaylist(w http.ResponseWriter, resp *http.Response, upstream string) {
	base, err := url.Parse(upstream)
	if err != nil {
		http.Error(w, "bad upstream url", http.StatusBadGateway)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, relayMaxPlaylistBytes+1))
	if err != nil {
		http.Error(w, "reading upstream playlist", http.StatusBadGateway)
		return
	}
	if len(body) > relayMaxPlaylistBytes {
		http.Error(w, "upstream playlist too large", http.StatusBadGateway)
		return
	}
	rewritten := r.rewritePlaylist(string(body), base)

	copyProxyHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Length", strconv.Itoa(len(rewritten)))
	w.WriteHeader(resp.StatusCode)
	_, _ = io.WriteString(w, rewritten)
}

// rewritePlaylist rewrites every URI reference in an HLS playlist body
// (plain segment/playlist lines, and quoted URI="..." tag attributes) to a
// fresh relay token, resolving relative references against base.
func (r *mediaRelay) rewritePlaylist(body string, base *url.URL) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "#"):
			if relayURIAttr.MatchString(line) {
				lines[i] = relayURIAttr.ReplaceAllStringFunc(line, func(m string) string {
					sub := relayURIAttr.FindStringSubmatch(m)
					return `URI="` + r.resolveAndRegister(sub[1], base) + `"`
				})
			}
		case strings.TrimSpace(line) == "":
			// blank line, pass through unchanged
		default:
			lines[i] = r.resolveAndRegister(line, base)
		}
	}
	return strings.Join(lines, "\n")
}

// resolveAndRegister resolves ref against base and registers it for a fresh
// relay token, returning that token's URL. On any error the original ref is
// returned unchanged (best-effort; the eventual fetch through the relay
// simply 404s or fails rather than corrupting the playlist).
func (r *mediaRelay) resolveAndRegister(ref string, base *url.URL) string {
	refURL, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	resolved := base.ResolveReference(refURL).String()
	token, err := r.register(resolved)
	if err != nil {
		return ref
	}
	return token
}

// copyProxyHeaders copies every header from src to dst except hop-by-hop
// headers, which must never be forwarded verbatim across a proxy boundary.
func copyProxyHeaders(dst, src http.Header) {
	for k, vv := range src {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// relayInput returns the URL ffmpeg/ffprobe should be given for mediaURL,
// plus the -protocol_whitelist value to pair with it.
//
// mediaURL matching selfBase (this server's own origin — see
// validateRemoteURL's selfBase carve-out) is passed through directly: it's
// already loopback-only and this process is both ends of that request, so
// relaying it would just add a hop. Every other (remote, untrusted) mediaURL
// is routed through globalRelay instead of being handed to ffmpeg/ffprobe
// directly (SEC-4): the relay is always plain loopback HTTP, so its
// protocol_whitelist is deliberately narrow ("http,tcp" — no https/tls/
// crypto, and never "file", which would reopen the local-file-read path
// validateRemoteURL exists to close).
func relayInput(mediaURL, selfBase string) (inputURL, protocolWhitelist string, err error) {
	localized := localize(mediaURL)
	if selfBase != "" && (localized == selfBase || strings.HasPrefix(localized, selfBase+"/")) {
		return localized, "http,https,tcp,tls,crypto", nil
	}
	tok, err := globalRelay.register(localized)
	if err != nil {
		return "", "", fmt.Errorf("media relay: %w", err)
	}
	return tok, "http,tcp", nil
}
