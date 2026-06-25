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
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/types"
)

const chunkSize = 65536 // 64 KiB — OpenSubtitles hash window

// prober is the concrete implementation of types.MediaProber.
type prober struct {
	// baseURLLocal is the local server URL (e.g. "http://127.0.0.1:11470"),
	// used to prefix scheme-less stream URLs passed to Probe.
	baseURLLocal string
	hls          *hlsManager
}

// New returns a MediaProber backed by system ffprobe/ffmpeg.
// baseURLLocal should include scheme and host with no trailing slash
// (e.g. "http://127.0.0.1:11470").
func New(baseURLLocal string) types.MediaProber {
	return &prober{baseURLLocal: strings.TrimRight(baseURLLocal, "/"), hls: newHLS()}
}

// Probe runs ffprobe on streamURL and returns the parsed JSON map.
// If streamURL has no scheme it is prefixed with p.baseURLLocal.
// A 30-second context timeout is applied to the child process.
func (p *prober) Probe(streamURL string) (interface{}, error) {
	if !strings.Contains(streamURL, "://") {
		streamURL = p.baseURLLocal + "/" + strings.TrimLeft(streamURL, "/")
	}

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
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("ffprobe json decode: %w", err)
	}
	return result, nil
}

// Tracks returns embedded non-video stream metadata for rawURL.
// It runs ffprobe -show_streams and returns a JSON-compatible slice of maps
// (one per audio or subtitle stream) in the shape:
//
//	{ "id":<stream_index>, "type":"audio"|"subtitle", "codec":<codec_name>,
//	  "lang":<tags.language>, "label":<tags.title>, "channels":<channels> }
//
// Scheme-less URLs are prefixed with p.baseURLLocal.
// The loopback HTTPS→HTTP rewrite (localize) is applied so ffprobe can read
// self-signed TLS streams.
func (p *prober) Tracks(rawURL string) (interface{}, error) {
	streamURL := rawURL
	if !strings.Contains(streamURL, "://") {
		streamURL = p.baseURLLocal + "/" + strings.TrimLeft(streamURL, "/")
	}
	// Rewrite loopback HTTPS to plain HTTP (ffprobe rejects self-signed certs).
	streamURL = localize(streamURL)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffprobe",
		"-v", "quiet", "-print_format", "json", "-show_streams", streamURL,
	).Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
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
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, fmt.Errorf("ffprobe json decode: %w", err)
	}
	result := make([]interface{}, 0, len(r.Streams))
	for _, st := range r.Streams {
		if st.CodecType == "video" {
			continue
		}
		result = append(result, map[string]interface{}{
			"id":       st.Index,
			"type":     st.CodecType,
			"codec":    st.CodecName,
			"lang":     st.Tags.Language,
			"label":    st.Tags.Title,
			"channels": st.Channels,
		})
	}
	return result, nil
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
// Supported URL schemes:
//   - http / https — resolved via Range requests (HEAD for size, then GET).
//   - file:// or a bare path — resolved via os.Open.
func (p *prober) OpenSubHash(videoURL string) (interface{}, error) {
	var (
		size       int64
		head, tail []byte
		err        error
	)

	if isHTTP(videoURL) {
		size, head, tail, err = fetchHTTPChunks(videoURL)
	} else {
		size, head, tail, err = readLocalChunks(toLocalPath(videoURL))
	}
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

// isHTTP reports whether u starts with http:// or https://.
func isHTTP(u string) bool {
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

// toLocalPath strips a file:// prefix, returning a bare filesystem path.
func toLocalPath(u string) string {
	return strings.TrimPrefix(u, "file://")
}

// fetchHTTPChunks fetches the first and last 64 KiB of a remote file
// using HTTP Range requests, returning (size, head, tail, err).
func fetchHTTPChunks(url string) (size int64, head, tail []byte, err error) {
	hClient := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodHead, url, nil)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("HEAD %s: %w", url, err)
	}
	resp, err := hClient.Do(req)
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
// A 15-second timeout is applied; the body is capped at chunkSize+1 bytes so
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

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("range GET %s: unexpected status %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, chunkSize+1))
}

// readLocalChunks reads the first and last 64 KiB from a local file path.
// If the file is smaller than 64 KiB, the same bytes are returned for
// both head and tail (consistent with the OpenSubtitles reference impl).
func readLocalChunks(path string) (size int64, head, tail []byte, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, nil, nil, err
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return 0, nil, nil, err
	}
	size = fi.Size()

	head = make([]byte, chunkSize)
	n, rerr := io.ReadFull(f, head)
	if rerr == io.ErrUnexpectedEOF || rerr == io.EOF {
		// File fits entirely within one chunk.
		head = head[:n]
		return size, head, head, nil
	}
	if rerr != nil {
		return 0, nil, nil, rerr
	}

	tailStart := size - chunkSize
	if tailStart < 0 {
		tailStart = 0
	}
	if _, err = f.Seek(tailStart, io.SeekStart); err != nil {
		return 0, nil, nil, err
	}

	tail = make([]byte, chunkSize)
	n, rerr = io.ReadFull(f, tail)
	if rerr == io.ErrUnexpectedEOF || rerr == io.EOF {
		tail = tail[:n]
	} else if rerr != nil {
		return 0, nil, nil, rerr
	}

	return size, head, tail, nil
}
