// Package media implements types.MediaProber, backing the ffprobe/ffmpeg helper
// routes. All external I/O is done via os/exec (ffprobe) or the standard
// net/http client; no third-party dependencies are required.
package media

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/netguard"
	"github.com/M0Rf30/stremio-server-go/internal/types"
)

const chunkSize = 65536 // 64 KiB — OpenSubtitles hash window

// probeTracksCacheMaxSize is the maximum number of entries kept in each of
// prober's probeCache and tracksCache.  Expired entries are swept on every
// insert; when the map still exceeds this limit after the TTL sweep, the
// soonest-expiring entry is evicted until it's back under the cap.
const probeTracksCacheMaxSize = 512

// openSubClient is a shared HTTP client for OpenSubHash (HEAD + Range GETs) and
// subtitle fetches. One transport means all requests to the same host reuse a
// single TCP connection pool instead of allocating a fresh one per call. The
// dialer's Control hook re-validates the resolved IP at connect time, closing
// the DNS-rebinding TOCTOU gap left by the validateRemoteURL pre-flight check.
var openSubClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 10 * time.Second,
			Control: netguard.DialControl(true),
		}).DialContext,
	},
}

// probeResultEntry holds a cached Probe() result (or error) with a wall-clock expiry.
type probeResultEntry struct {
	result    interface{}
	err       error
	expiresAt time.Time
}

// tracksCacheEntry holds a cached Tracks() result (or error) with a wall-clock expiry.
type tracksCacheEntry struct {
	result    interface{}
	err       error
	expiresAt time.Time
}

// prober is the concrete implementation of types.MediaProber.
type prober struct {
	// baseURLLocal is the local server URL (e.g. "http://127.0.0.1:11470"),
	// used to prefix scheme-less stream URLs passed to Probe.
	baseURLLocal string
	hls          *hlsManager
	// Probe and Tracks result caches keyed by resolved URL; short-TTL prevents
	// repeated ffprobe spawns for the same URL across consecutive player requests.
	probeMu     sync.Mutex
	probeCache  map[string]probeResultEntry
	tracksMu    sync.Mutex
	tracksCache map[string]tracksCacheEntry
}

// New returns a MediaProber backed by system ffprobe/ffmpeg.
// baseURLLocal should include scheme and host with no trailing slash
// (e.g. "http://127.0.0.1:11470").
func New(baseURLLocal string) types.MediaProber {
	base := strings.TrimRight(baseURLLocal, "/")
	return &prober{
		baseURLLocal: base,
		hls:          newHLS(base),
		probeCache:   make(map[string]probeResultEntry), // must be non-nil before first write
		tracksCache:  make(map[string]tracksCacheEntry), // must be non-nil before first write
	}
}

// Probe runs ffprobe on streamURL and returns the parsed JSON map.
// If streamURL has no scheme it is prefixed with p.baseURLLocal.
// A 30-second context timeout is applied to the child process.
func (p *prober) Probe(streamURL string) (interface{}, error) {
	if strings.Contains(streamURL, "://") {
		if err := validateRemoteURL(streamURL, p.baseURLLocal); err != nil {
			return nil, err
		}
	} else {
		streamURL = p.baseURLLocal + "/" + strings.TrimLeft(streamURL, "/")
	}

	// Serve from cache — avoids repeated ffprobe spawns for the same URL.
	p.probeMu.Lock()
	if e, ok := p.probeCache[streamURL]; ok && time.Now().Before(e.expiresAt) {
		p.probeMu.Unlock()
		return e.result, e.err
	}
	p.probeMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		streamURL,
	)

	out, err := cmd.Output()
	var result map[string]interface{}
	if err != nil {
		err = fmt.Errorf("ffprobe: %w", err)
	} else if jErr := json.Unmarshal(out, &result); jErr != nil {
		err = fmt.Errorf("ffprobe json decode: %w", jErr)
		result = nil
	}

	// Cache with TTL: 5 min on success, 30 s on error (bounds broken-URL hammering).
	p.probeMu.Lock()
	if len(p.probeCache) >= probeTracksCacheMaxSize {
		now := time.Now()
		for k, v := range p.probeCache {
			if now.After(v.expiresAt) {
				delete(p.probeCache, k)
			}
		}
	}
	// Hard size cap: evict soonest-expiring entry until under limit.
	for len(p.probeCache) >= probeTracksCacheMaxSize {
		var evict string
		var evictExp time.Time
		for k, v := range p.probeCache {
			if evict == "" || v.expiresAt.Before(evictExp) {
				evict = k
				evictExp = v.expiresAt
			}
		}
		delete(p.probeCache, evict)
	}
	ttl := 5 * time.Minute
	if err != nil {
		ttl = 30 * time.Second
	}
	p.probeCache[streamURL] = probeResultEntry{result: result, err: err, expiresAt: time.Now().Add(ttl)}
	p.probeMu.Unlock()

	return result, err
}

// Tracks returns embedded non-video stream metadata for rawURL.
// It runs ffprobe -show_streams and returns a JSON-compatible slice of maps
// (one per audio or subtitle stream) in the shape:
//
//	{ "id":<stream_index>, "type":"audio"|"subtitle", "codec":<codec_name>,
//	  "lang":<tags.language>, "label":<tags.title>, "channels":<channels> }
//
// Scheme-less URLs are prefixed with p.baseURLLocal; URLs that carry their own
// scheme are validated (http/https only, non-private host) before reaching
// ffprobe, exactly as in Probe.
// The loopback HTTPS→HTTP rewrite (localize) is applied so ffprobe can read
// self-signed TLS streams.
func (p *prober) Tracks(rawURL string) (interface{}, error) {
	streamURL := rawURL
	if strings.Contains(streamURL, "://") {
		if err := validateRemoteURL(streamURL, p.baseURLLocal); err != nil {
			return nil, err
		}
	} else {
		streamURL = p.baseURLLocal + "/" + strings.TrimLeft(streamURL, "/")
	}
	// Rewrite loopback HTTPS to plain HTTP (ffprobe rejects self-signed certs).
	streamURL = localize(streamURL)

	// Serve from cache — avoids repeated ffprobe spawns for the same URL.
	p.tracksMu.Lock()
	if e, ok := p.tracksCache[streamURL]; ok && time.Now().Before(e.expiresAt) {
		p.tracksMu.Unlock()
		return e.result, e.err
	}
	p.tracksMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffprobe",
		"-v", "quiet", "-print_format", "json", "-show_streams", streamURL,
	).Output()

	var result interface{}
	if err != nil {
		err = fmt.Errorf("ffprobe: %w", err)
	} else {
		var r struct {
			Streams []struct {
				Index     int    `json:"index"`
				CodecType string `json:"codec_type"`
				CodecName string `json:"codec_name"`
				Channels  int    `json:"channels"`
				Tags      struct {
					Language string `json:"language"`
					Title    string `json:"title"`
				} `json:"tags"`
			} `json:"streams"`
		}
		if jErr := json.Unmarshal(out, &r); jErr != nil {
			err = fmt.Errorf("ffprobe json decode: %w", jErr)
		} else {
			tracks := make([]interface{}, 0, len(r.Streams))
			for _, st := range r.Streams {
				if st.CodecType == "video" {
					continue
				}
				tracks = append(tracks, map[string]interface{}{
					"id":       st.Index,
					"type":     st.CodecType,
					"codec":    st.CodecName,
					"lang":     st.Tags.Language,
					"label":    st.Tags.Title,
					"channels": st.Channels,
				})
			}
			result = tracks
		}
	}

	// Cache with TTL: 5 min on success, 30 s on error.
	p.tracksMu.Lock()
	if len(p.tracksCache) >= probeTracksCacheMaxSize {
		now := time.Now()
		for k, v := range p.tracksCache {
			if now.After(v.expiresAt) {
				delete(p.tracksCache, k)
			}
		}
	}
	// Hard size cap: evict soonest-expiring entry until under limit.
	for len(p.tracksCache) >= probeTracksCacheMaxSize {
		var evict string
		var evictExp time.Time
		for k, v := range p.tracksCache {
			if evict == "" || v.expiresAt.Before(evictExp) {
				evict = k
				evictExp = v.expiresAt
			}
		}
		delete(p.tracksCache, evict)
	}
	ttl := 5 * time.Minute
	if err != nil {
		ttl = 30 * time.Second
	}
	p.tracksCache[streamURL] = tracksCacheEntry{result: result, err: err, expiresAt: time.Now().Add(ttl)}
	p.tracksMu.Unlock()

	return result, err
}

// OpenSubHash computes the OpenSubtitles 64-bit file hash for videoURL.
//
// Algorithm (per the OpenSubtitles spec):
//
//	hash = (filesize
//	        + Σ uint64LE over first 64 KiB
//	        + Σ uint64LE over last  64 KiB) mod 2^64
//
// The result is formatted as a 16-character lower-case hex string.
// Returns map[string]interface{}{"hash": <hex>, "size": <int64>}.
//
// Only http/https URLs on a public host are accepted (validateRemoteURL);
// videoURL never reaches the filesystem, closing the arbitrary local-file-read
// this endpoint was directly exposed to via the unauthenticated
// /opensubHash?videoUrl= query parameter.
func (p *prober) OpenSubHash(videoURL string) (interface{}, error) {
	if err := validateRemoteURL(videoURL, p.baseURLLocal); err != nil {
		return nil, err
	}

	size, head, tail, err := fetchHTTPChunks(videoURL)
	if err != nil {
		return nil, err
	}

	h := computeOpenSubHash(size, head, tail)
	return map[string]interface{}{
		"hash": fmt.Sprintf("%016x", h),
		"size": size,
	}, nil
}

// computeOpenSubHash accumulates the uint64 checksum.
// Natural uint64 overflow implements mod 2^64.
func computeOpenSubHash(size int64, head, tail []byte) uint64 {
	var h uint64
	h += uint64(size)
	for i := 0; i+8 <= len(head); i += 8 {
		h += binary.LittleEndian.Uint64(head[i : i+8])
	}
	for i := 0; i+8 <= len(tail); i += 8 {
		h += binary.LittleEndian.Uint64(tail[i : i+8])
	}
	return h
}

// validateRemoteURL is the single SSRF/local-file-read choke point for every
// externally-supplied media URL that reaches an ffprobe/ffmpeg subprocess or
// an outbound HTTP client (Probe, StartHLS, OpenSubHash, fetchSubBytes). Only
// http/https schemes are accepted — file://, pipe:, concat:, data:, bare
// paths, and every other ffmpeg-supported protocol are rejected outright —
// and the resolved host must not be a private, loopback, link-local, or
// cloud-metadata address (checked via netguard.ValidateIP). IP-literal hosts
// are checked directly with no DNS lookup; hostnames are resolved and every
// returned address is checked, so a DNS answer mixing public and private
// addresses is still rejected.
//
// selfBase (this server's own local base URL, e.g. "http://127.0.0.1:11470")
// is exempted from the IP check: when the HTTPS UI on :12470 asks the server
// to transcode or probe a stream the server itself is serving on :11470, the
// media URL legitimately points at loopback. Only that exact origin is
// allowed — every other loopback or private target is still rejected — and
// localize() is applied first so the self-signed https://…:12470 form maps
// onto the same origin. Pass "" to disallow self-references entirely.
func validateRemoteURL(raw, selfBase string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported URL scheme %q (only http/https allowed)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL %q has no host", raw)
	}
	if selfBase != "" {
		if l := localize(raw); l == selfBase || strings.HasPrefix(l, selfBase+"/") {
			return nil
		}
	}
	ips := []net.IP{net.ParseIP(host)}
	if ips[0] == nil {
		if ips, err = net.LookupIP(host); err != nil {
			return fmt.Errorf("resolving host %q: %w", host, err)
		}
	}
	for _, ip := range ips {
		if err := netguard.ValidateIP(ip, true); err != nil {
			return fmt.Errorf("URL %q: %w", raw, err)
		}
	}
	return nil
}

// fetchHTTPChunks fetches the first and last 64 KiB of a remote file
// using HTTP Range requests, returning (size, head, tail, err).
func fetchHTTPChunks(url string) (size int64, head, tail []byte, err error) {
	// openSubClient: shared transport reuses the TCP connection for HEAD + 2× Range GETs.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodHead, url, nil)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("HEAD %s: %w", url, err)
	}
	resp, err := openSubClient.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("HEAD %s: %w", url, err)
	}
	_ = resp.Body.Close()

	if resp.ContentLength <= 0 {
		return 0, nil, nil, fmt.Errorf("cannot determine Content-Length for %s", url)
	}
	size = resp.ContentLength

	head, err = httpRangeGet(url, 0, int64(chunkSize)-1)
	if err != nil {
		return 0, nil, nil, err
	}

	tailStart := size - chunkSize
	if tailStart < 0 {
		tailStart = 0
	}
	tail, err = httpRangeGet(url, tailStart, size-1)
	if err != nil {
		return 0, nil, nil, err
	}

	return size, head, tail, nil
}

// httpRangeGet performs a Range GET [from, to] and returns the body.
// A 15-second context timeout is applied; the body is capped at chunkSize+1 bytes so
// a server that ignores the Range header and returns a 200 + full body
// cannot cause an OOM.
func httpRangeGet(url string, from, to int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, to))

	// openSubClient: shared transport; per-request timeout applied via ctx.
	resp, err := openSubClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("range GET %s: unexpected status %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, chunkSize+1))
}
