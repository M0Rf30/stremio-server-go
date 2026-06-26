package media

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// segDur is the HLS segment length in seconds.
const segDur = 4.0

// videoGOP is the fixed keyframe interval for all video encodes.
// Computed as round(30fps × 4s) = 120.  Aligns segment boundaries with IDR
// frames so the player can seek without decoding a preceding GOP.
const videoGOP = 120

// Bitrate caps applied to all video transcode paths.
const (
	videoBitrateCap = "8M"
	videoMaxrate    = "8M"
	videoBufsize    = "16M"
)

// segKind identifies what streams a transcoded segment contains.
type segKind int

const (
	segMuxed     segKind = iota // video + first-audio, muxed (single-audio session)
	segVideoOnly                // video only, no audio (multi-audio session)
	segAudioOnly                // audio only for one track (multi-audio session)
)

// Lifecycle tuning constants.
const (
	// sessionTTL is the idle-eviction window: sessions not accessed within
	// this duration are removed by the background reaper.
	sessionTTL = 60 * time.Second
	// reaperInterval is how frequently the reaper scans for idle sessions.
	reaperInterval = 30 * time.Second
	// negProbeTTL is how long a failed or zero-duration ffprobe result is
	// cached to prevent hammering a broken URL with repeated 30-second probes.
	negProbeTTL = 5 * time.Minute
)

// audioStream describes one audio track discovered by ffprobe.
type audioStream struct {
	Index     int    // global ffprobe stream index
	CodecName string // e.g. "ac3", "aac"
	Channels  int
	Language  string // tags.language (BCP-47, may be empty)
	Title     string // tags.title (may be empty)
	IsDefault bool   // disposition.default != 0
}

// subtitleStream describes one text-based subtitle track discovered by ffprobe.
// Image-based subtitle formats (pgssub, dvdsub, xsub, dvb_subtitle,
// hdmv_pgs_subtitle) are excluded: they cannot be converted to WebVTT text
// and require graphical composition — this is a format limitation, not a stub.
type subtitleStream struct {
	Index     int    // global ffprobe stream index
	SubIdx    int    // 0-based index among subtitle streams in the file
	CodecName string // e.g. "subrip", "ass", "webvtt", "mov_text"
	Language  string // tags.language (BCP-47, may be empty)
	Title     string // tags.title (may be empty)
	IsDefault bool   // disposition.default != 0
}

// hlsSession holds the state for one transcoded stream (keyed by client id).
// Segments are transcoded on demand so the client can seek anywhere; a VOD
// playlist computed from the media duration lists every segment up front.
type hlsSession struct {
	mediaURL        string
	dir             string
	mu              sync.RWMutex
	duration        float64
	audioStreams    []audioStream          // probed once in StartHLS; nil until then
	subtitleStreams []subtitleStream       // text-based subtitle tracks (subrip/ass/ssa/mov_text/webvtt)
	multiAudio      bool                   // true when len(audioStreams) >= 2
	highBitDepth    bool                   // true if any video stream is 10/12-bit
	segLocks        map[string]*sync.Mutex // keyed by segment filename
	lastAccess      atomic.Int64           // unix nanoseconds; updated on each StartHLS/HLSFile call
}

// hwEncoder holds the selected H.264 encoder identity plus any device path
// required for it.  Software fallback is codec="libx264", isHW=false.
type hwEncoder struct {
	codec     string // ffmpeg codec name: "h264_vaapi", "h264_nvenc", "libx264", …
	isHW      bool   // false only for libx264
	driDevice string // VAAPI renderD* path; empty for all non-VAAPI codecs
}

// probeCacheEntry records a cached ffprobe result for negative caching.
// Used to short-circuit repeated 30-second probes of broken or unreachable URLs.
type probeCacheEntry struct {
	result    probeMediaResult
	expiresAt time.Time
}

// hlsManager owns per-id sessions and the hardware-accel decision.
// enc is set once in newHLS() and is read-only thereafter.
type hlsManager struct {
	base string
	enc  hwEncoder

	mu           sync.Mutex
	sessions     map[string]*hlsSession
	probeCache   map[string]probeCacheEntry // negative-probe cache; keyed by mediaURL
	transcodeSem chan struct{}              // bounds concurrent ffmpeg transcode spawns
	stopCh       chan struct{}              // closed by CloseHLS to stop the reaper
}

// ── encoder detection ─────────────────────────────────────────────────────────

// encListOnce guards the one-time run of `ffmpeg -hide_banner -encoders`.
var (
	encListOnce sync.Once
	encListOut  string
)

// encodersList returns the cached stdout of `ffmpeg -hide_banner -encoders`.
// The command is run exactly once per process; subsequent calls are instant.
func encodersList() string {
	encListOnce.Do(func() {
		out, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
		if err == nil {
			encListOut = string(out)
		}
	})
	return encListOut
}

// verifyEncoder confirms an encoder actually works on this hardware by running
// a 5-second test-transcode of a synthetic 64×64 source into /dev/null.
//
//   - preInput  — args inserted before -i (e.g. -vaapi_device /dev/dri/renderD128)
//   - preEncode — args inserted before -c:v (e.g. -vf format=nv12,hwupload)
//
// Returns true only if ffmpeg exits 0 within the timeout.
func verifyEncoder(codec string, preInput, preEncode []string) bool {
	args := []string{"-hide_banner", "-loglevel", "error"}
	args = append(args, preInput...)
	args = append(args,
		"-f", "lavfi", "-i", "testsrc2=size=64x64:rate=1:duration=1",
		"-frames:v", "1", "-an",
	)
	args = append(args, preEncode...)
	args = append(args, "-c:v", codec, "-f", "null", "-")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "ffmpeg", args...).Run() == nil
}

// selectEncoder probes available H.264 encoders and returns the best one that
// actually works on this machine.  Called once in newHLS().
//
// Selection rules:
//   - STREMIO_HWACCEL=0                            → always libx264 (hard switch)
//   - STREMIO_HWACCEL=nvenc|qsv|vaapi|…            → explicit; listed encoder used
//     without the verify step (user accepts responsibility)
//   - STREMIO_HWACCEL="" or "auto" (default)       → auto; only verified encoders
//     chosen; priority: NVENC > QSV > VideoToolbox > VAAPI > V4L2M2M > libx264
func selectEncoder() hwEncoder {
	sw := hwEncoder{codec: "libx264"} // software fallback

	// Hard software-only override.
	if os.Getenv("STREMIO_HWACCEL") == "0" {
		return sw
	}

	out := encodersList()
	listed := func(enc string) bool { return strings.Contains(out, enc) }

	// Explicit profile override: use the requested encoder if listed.
	switch strings.ToLower(strings.TrimSpace(os.Getenv("STREMIO_HWACCEL"))) {
	case "nvenc", "nvidia", "cuda":
		if listed("h264_nvenc") {
			return hwEncoder{codec: "h264_nvenc", isHW: true}
		}
		return sw
	case "qsv", "intel", "quicksync":
		if listed("h264_qsv") {
			return hwEncoder{codec: "h264_qsv", isHW: true}
		}
		return sw
	case "vaapi":
		if listed("h264_vaapi") {
			return hwEncoder{codec: "h264_vaapi", isHW: true, driDevice: "/dev/dri/renderD128"}
		}
		return sw
	case "videotoolbox", "vt":
		if listed("h264_videotoolbox") {
			return hwEncoder{codec: "h264_videotoolbox", isHW: true}
		}
		return sw
	case "v4l2", "v4l2m2m":
		if listed("h264_v4l2m2m") {
			return hwEncoder{codec: "h264_v4l2m2m", isHW: true}
		}
		return sw
		// "" / "auto" / anything else: fall through to auto-detect.
	}

	// Auto-detect: run a real test-transcode for each candidate before selecting.
	// "Listed in ffmpeg -encoders" only means the codec was compiled in; it does
	// not mean the local driver, device, or session limits can actually open it.

	// NVENC (NVIDIA CUDA)
	if listed("h264_nvenc") && verifyEncoder("h264_nvenc", nil, nil) {
		return hwEncoder{codec: "h264_nvenc", isHW: true}
	}
	// Intel Quick Sync Video
	if listed("h264_qsv") && verifyEncoder("h264_qsv", nil, nil) {
		return hwEncoder{codec: "h264_qsv", isHW: true}
	}
	// Apple VideoToolbox (macOS)
	if listed("h264_videotoolbox") && verifyEncoder("h264_videotoolbox", nil, nil) {
		return hwEncoder{codec: "h264_videotoolbox", isHW: true}
	}
	// VAAPI (Linux: Intel Iris Xe, AMD, etc.) — needs the device path for both
	// the verification test-transcode and the real encode.
	const driDev = "/dev/dri/renderD128"
	if listed("h264_vaapi") {
		if _, statErr := os.Stat(driDev); statErr == nil {
			preIn := []string{"-vaapi_device", driDev}
			preEnc := []string{"-vf", "format=nv12,hwupload"}
			if verifyEncoder("h264_vaapi", preIn, preEnc) {
				return hwEncoder{codec: "h264_vaapi", isHW: true, driDevice: driDev}
			}
		}
	}
	// V4L2 Memory-to-Memory (ARM / Raspberry Pi)
	if listed("h264_v4l2m2m") && verifyEncoder("h264_v4l2m2m", nil, nil) {
		return hwEncoder{codec: "h264_v4l2m2m", isHW: true}
	}

	return sw // no working hardware encoder found
}

func newHLS() *hlsManager {
	base := filepath.Join(os.TempDir(), "stremio-hls")
	_ = os.MkdirAll(base, 0o755)
	n := runtime.NumCPU()
	if n < 1 {
		n = 1
	}
	enc := selectEncoder()
	if enc.isHW {
		log.Printf("media: HLS transcode using hardware encoder %q (device %q)", enc.codec, enc.driDevice)
	} else {
		log.Printf("media: HLS transcode using SOFTWARE encoder %q — no hardware acceleration active; expect high CPU. Set STREMIO_HWACCEL=vaapi (Intel/AMD on Linux) and ensure /dev/dri access, or pass --device /dev/dri in a container.", enc.codec)
	}
	m := &hlsManager{
		base:         base,
		enc:          enc,
		sessions:     map[string]*hlsSession{},
		probeCache:   map[string]probeCacheEntry{},
		transcodeSem: make(chan struct{}, n),
		stopCh:       make(chan struct{}),
	}
	go m.reaper()
	return m
}

// localize rewrites the self-signed https loopback URL to plain http so ffmpeg
// (which doesn't ignore TLS errors) can read the stream.
func localize(u string) string {
	return strings.ReplaceAll(u, "https://127.0.0.1:12470", "http://127.0.0.1:11470")
}

// ── probeMedia ────────────────────────────────────────────────────────────────

// probeMediaResult carries the output of a combined ffprobe run.
type probeMediaResult struct {
	duration        float64
	audioStreams    []audioStream
	subtitleStreams []subtitleStream
	highBitDepth    bool // true if any video stream is 10/12-bit
}

// probeMedia runs a single ffprobe with -show_format -show_streams and returns
// the media duration, every audio stream, and whether the video is high-bit-depth.
// A 30-second timeout is applied over the provided ctx.
// The caller must NOT hold any session lock when calling this function.
func probeMedia(ctx context.Context, mediaURL string) probeMediaResult {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffprobe",
		"-v", "quiet", "-print_format", "json",
		"-show_format", "-show_streams",
		localize(mediaURL)).Output()
	if err != nil {
		return probeMediaResult{}
	}
	var r struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			Index     int    `json:"index"`
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			PixFmt    string `json:"pix_fmt"` // e.g. "yuv420p", "yuv420p10le"
			Profile   string `json:"profile"` // e.g. "High", "Main 10"
			Channels  int    `json:"channels"`
			Tags      struct {
				Language string `json:"language"`
				Title    string `json:"title"`
			} `json:"tags"`
			Disposition struct {
				Default int `json:"default"`
			} `json:"disposition"`
		} `json:"streams"`
	}
	if json.Unmarshal(out, &r) != nil {
		return probeMediaResult{}
	}
	d, _ := strconv.ParseFloat(r.Format.Duration, 64)
	var audio []audioStream
	var subs []subtitleStream
	var subCount int // tracks 0-based index among subtitle streams
	var highBit bool
	for _, st := range r.Streams {
		switch st.CodecType {
		case "audio":
			audio = append(audio, audioStream{
				Index:     st.Index,
				CodecName: st.CodecName,
				Channels:  st.Channels,
				Language:  st.Tags.Language,
				Title:     st.Tags.Title,
				IsDefault: st.Disposition.Default != 0,
			})
		case "video":
			// Detect 10/12-bit sources:
			//   pix_fmt: "yuv420p10le", "yuv422p12le", "p010le", etc. contain "10"/"12".
			//   profile:  "Main 10" (HEVC/AVC Hi10P) contains "10".
			// Common 8-bit formats ("yuv420p", "nv12", "yuvj420p", …) do not match.
			pf := strings.ToLower(st.PixFmt)
			pr := strings.ToLower(st.Profile)
			if strings.Contains(pf, "10") || strings.Contains(pf, "12") ||
				strings.Contains(pr, "10") {
				highBit = true
			}
		case "subtitle":
			// Only include text-based subtitle codecs that ffmpeg can convert to WebVTT.
			// Image-based formats (pgssub / dvdsub / xsub / dvb_subtitle /
			// hdmv_pgs_subtitle) are skipped — they cannot be losslessly text-converted.
			switch strings.ToLower(st.CodecName) {
			case "subrip", "srt", "ass", "ssa", "mov_text", "webvtt", "text", "jacosub", "realtext", "sami", "subviewer":
				subs = append(subs, subtitleStream{
					Index:     st.Index,
					SubIdx:    subCount,
					CodecName: st.CodecName,
					Language:  st.Tags.Language,
					Title:     st.Tags.Title,
					IsDefault: st.Disposition.Default != 0,
				})
			}
			subCount++ // always advance so SubIdx matches ffprobe's 0:s:<k> numbering
		}
	}
	return probeMediaResult{duration: d, audioStreams: audio, subtitleStreams: subs, highBitDepth: highBit}
}

// ── StartHLS ──────────────────────────────────────────────────────────────────

// StartHLS registers a session for id (probing duration + audio/subtitle streams
// once) and returns the master playlist text.
//
//   - <=1 audio track, no text subs: single muxed variant pointing at playlist.m3u8.
//   - <=1 audio track, text subs present: muxed variant with SUBTITLES="subs".
//   - >=2 audio tracks: EXT-X-MEDIA TYPE=AUDIO group + single video.m3u8 variant;
//     text subtitle renditions are added alongside when present.
//
// Text subtitle renditions: one EXT-X-MEDIA:TYPE=SUBTITLES entry per text
// subtitle track. The URI points at sub<k>.m3u8 which serves a single-segment
// WebVTT playlist.  sub<k>.vtt is extracted from the container on first request.
func (m *hlsManager) StartHLS(id, mediaURL string) (string, error) {
	if mediaURL == "" {
		return "", fmt.Errorf("hls: missing mediaURL")
	}
	// Reject ids that could escape the base directory via path traversal.
	if id == "" || id != filepath.Base(id) || strings.Contains(id, "..") {
		return "", fmt.Errorf("hls: invalid session id %q", id)
	}
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok {
		s = &hlsSession{
			mediaURL: mediaURL,
			dir:      filepath.Join(m.base, id),
			segLocks: map[string]*sync.Mutex{},
		}
		s.lastAccess.Store(time.Now().UnixNano())
		_ = os.MkdirAll(s.dir, 0o755)
		m.sessions[id] = s
	}
	m.mu.Unlock()

	// Refresh lastAccess for both new and reused sessions.
	s.lastAccess.Store(time.Now().UnixNano())

	// Probe outside the session write lock so a 30-second ffprobe does not stall
	// concurrent HLSFile calls on the same session.
	s.mu.RLock()
	needProbe := s.duration == 0
	s.mu.RUnlock()

	if needProbe {
		// Negative-probe cache: avoid hammering a broken URL with repeated 30s probes.
		m.mu.Lock()
		cached, hasCached := m.probeCache[mediaURL]
		m.mu.Unlock()

		var res probeMediaResult
		if hasCached && time.Now().Before(cached.expiresAt) {
			res = cached.result
		} else {
			res = probeMedia(context.Background(), mediaURL)
			m.mu.Lock()
			if res.duration == 0 {
				// Cache the negative result to short-circuit future probes.
				m.probeCache[mediaURL] = probeCacheEntry{
					result:    res,
					expiresAt: time.Now().Add(negProbeTTL),
				}
			} else {
				// Positive result — clear any stale negative entry for this URL.
				delete(m.probeCache, mediaURL)
			}
			m.mu.Unlock()
		}

		// Store result under the session write lock; another goroutine racing
		// through StartHLS for the same id may have already stored a valid probe.
		s.mu.Lock()
		if s.duration == 0 {
			s.duration = res.duration
			s.audioStreams = res.audioStreams
			s.subtitleStreams = res.subtitleStreams
			s.multiAudio = len(s.audioStreams) >= 2
			s.highBitDepth = res.highBitDepth
		}
		s.mu.Unlock()
	}

	s.mu.RLock()
	multiAudio := s.multiAudio
	audioStreams := s.audioStreams
	subtitleStreams := s.subtitleStreams
	s.mu.RUnlock()

	hasSubs := len(subtitleStreams) > 0

	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:4\n")

	// ── Subtitle rendition group ──────────────────────────────────────────────
	// One EXT-X-MEDIA:TYPE=SUBTITLES entry per text subtitle stream.
	// The GROUP-ID "subs" is referenced in every EXT-X-STREAM-INF below.
	if hasSubs {
		for k, sub := range subtitleStreams {
			// Human-readable NAME: prefer title, then language, then ordinal.
			name := sub.Title
			if name == "" {
				name = sub.Language
			}
			if name == "" {
				name = fmt.Sprintf("Subtitle %d", k+1)
			}
			// LANGUAGE must be a BCP-47 tag; fall back to "und" when absent.
			lang := sub.Language
			if lang == "" {
				lang = "und"
			}
			isDefault := "NO"
			if k == 0 {
				isDefault = "YES"
			}
			fmt.Fprintf(&b,
				"#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"subs\",LANGUAGE=\"%s\",NAME=\"%s\",DEFAULT=%s,AUTOSELECT=YES,FORCED=NO,URI=\"sub%d.m3u8\"\n",
				lang, name, isDefault, k,
			)
		}
	}

	if !multiAudio {
		// Single-audio (or no-audio) path.
		if hasSubs {
			b.WriteString("#EXT-X-STREAM-INF:BANDWIDTH=4000000,CODECS=\"avc1.640029,mp4a.40.2\",SUBTITLES=\"subs\"\nplaylist.m3u8\n")
		} else {
			b.WriteString("#EXT-X-STREAM-INF:BANDWIDTH=4000000,CODECS=\"avc1.640029,mp4a.40.2\"\nplaylist.m3u8\n")
		}
		return b.String(), nil
	}

	// ── Multi-audio master playlist ───────────────────────────────────────────
	// Player audio-menu behaviour: the browser/AVPlayer sees each EXT-X-MEDIA
	// entry as a selectable audio rendition.  It follows the URI to audio<k>.m3u8
	// and fetches the corresponding a<k>seg<n>.ts files.

	// Determine which stream gets DEFAULT=YES (first with disposition.default,
	// or stream 0 if none is marked default).
	defaultIdx := 0
	for k, a := range audioStreams {
		if a.IsDefault {
			defaultIdx = k
			break
		}
	}

	for k, a := range audioStreams {
		// Human-readable NAME: prefer language tag, then title, then ordinal.
		name := a.Language
		if name == "" {
			name = a.Title
		}
		if name == "" {
			name = fmt.Sprintf("Audio %d", k+1)
		}
		defaultVal, autoVal := "NO", "NO"
		if k == defaultIdx {
			defaultVal, autoVal = "YES", "YES"
		}
		// Optional LANGUAGE attribute (BCP-47 tag forwarded from the container).
		langAttr := ""
		if a.Language != "" {
			langAttr = fmt.Sprintf(",LANGUAGE=\"%s\"", a.Language)
		}
		fmt.Fprintf(&b,
			"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\"%s,NAME=\"%s\",DEFAULT=%s,AUTOSELECT=%s,URI=\"audio%d.m3u8\"\n",
			langAttr, name, defaultVal, autoVal, k,
		)
	}

	// Single video-only variant; AUDIO="aud" links it to the audio rendition group.
	// SUBTITLES="subs" links it to the subtitle rendition group when subs are present.
	if hasSubs {
		b.WriteString("#EXT-X-STREAM-INF:BANDWIDTH=4000000,CODECS=\"avc1.640029,mp4a.40.2\",AUDIO=\"aud\",SUBTITLES=\"subs\"\nvideo.m3u8\n")
	} else {
		b.WriteString("#EXT-X-STREAM-INF:BANDWIDTH=4000000,CODECS=\"avc1.640029,mp4a.40.2\",AUDIO=\"aud\"\nvideo.m3u8\n")
	}
	return b.String(), nil
}

// ── HLSFile ───────────────────────────────────────────────────────────────────

// HLSFile serves the VOD playlist or transcodes the requested segment on demand.
//
// filepath.Base(name) is applied first so no sub-path traversal is possible.
// Flat file name dispatch:
//
//	playlist.m3u8   → muxed VOD playlist  (single-audio path; seg<n>.ts)
//	video.m3u8      → video-only VOD playlist (multi-audio; seg<n>.ts, video-only transcode)
//	audio<k>.m3u8   → audio-only VOD playlist for stream k (a<k>seg<n>.ts)
//	sub<k>.m3u8     → single-segment WebVTT subtitle playlist
//	sub<k>.vtt      → full subtitle extracted to WebVTT (cached)
//	seg<n>.ts       → muxed seg (single-audio) or video-only seg (multi-audio)
//	a<k>seg<n>.ts   → audio-only segment for stream k
func (m *hlsManager) HLSFile(ctx context.Context, id, name string) (string, string, error) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return "", "", fmt.Errorf("hls: unknown session %s", id)
	}
	// Touch lastAccess before any work so the reaper knows this session is active.
	s.lastAccess.Store(time.Now().UnixNano())

	name = filepath.Base(name)
	// Belt-and-suspenders: ensure the joined path stays inside the session directory.
	// filepath.Base already strips separators; this also catches name=="..".
	if p := filepath.Join(s.dir, name); !strings.HasPrefix(p, s.dir+string(os.PathSeparator)) {
		return "", "", fmt.Errorf("hls: path %q not within session directory", name)
	}
	// Snapshot guarded fields before per-file dispatch.
	s.mu.RLock()
	multiAudio := s.multiAudio
	audioStreams := s.audioStreams
	subtitleStreams := s.subtitleStreams
	s.mu.RUnlock()

	// ── playlists ────────────────────────────────────────────────────────────

	if name == "playlist.m3u8" {
		// Muxed playlist: segment names are seg<n>.ts (segPrefix="").
		p := filepath.Join(s.dir, "playlist.m3u8")
		if err := s.writePlaylist(p, ""); err != nil {
			return "", "", err
		}
		return p, "application/vnd.apple.mpegurl", nil
	}

	if name == "video.m3u8" {
		if !multiAudio {
			return "", "", fmt.Errorf("hls: video.m3u8 is only available in multi-audio sessions")
		}
		p := filepath.Join(s.dir, "video.m3u8")
		// Same segment timing as the muxed playlist; seg<n>.ts are video-only.
		if err := s.writePlaylist(p, ""); err != nil {
			return "", "", err
		}
		return p, "application/vnd.apple.mpegurl", nil
	}

	if strings.HasPrefix(name, "audio") && strings.HasSuffix(name, ".m3u8") {
		kStr := strings.TrimSuffix(strings.TrimPrefix(name, "audio"), ".m3u8")
		k, err := strconv.Atoi(kStr)
		if err != nil || k < 0 || k >= len(audioStreams) {
			return "", "", fmt.Errorf("hls: bad audio playlist %q", name)
		}
		p := filepath.Join(s.dir, name)
		// Audio segments are named a<k>seg<n>.ts (segPrefix="a<k>").
		if err := s.writePlaylist(p, fmt.Sprintf("a%d", k)); err != nil {
			return "", "", err
		}
		return p, "application/vnd.apple.mpegurl", nil
	}

	// sub<k>.m3u8 — single-segment WebVTT subtitle playlist for text subtitle k.
	if strings.HasPrefix(name, "sub") && strings.HasSuffix(name, ".m3u8") {
		kStr := strings.TrimSuffix(strings.TrimPrefix(name, "sub"), ".m3u8")
		k, err := strconv.Atoi(kStr)
		if err != nil || k < 0 || k >= len(subtitleStreams) {
			return "", "", fmt.Errorf("hls: bad subtitle playlist %q", name)
		}
		p := filepath.Join(s.dir, name)
		if err := s.writeSubPlaylist(p, k); err != nil {
			return "", "", err
		}
		return p, "application/vnd.apple.mpegurl", nil
	}

	// sub<k>.vtt — full subtitle track extracted to WebVTT on first request.
	if strings.HasPrefix(name, "sub") && strings.HasSuffix(name, ".vtt") {
		kStr := strings.TrimSuffix(strings.TrimPrefix(name, "sub"), ".vtt")
		k, err := strconv.Atoi(kStr)
		if err != nil || k < 0 || k >= len(subtitleStreams) {
			return "", "", fmt.Errorf("hls: bad subtitle track %q", name)
		}
		p, err := m.extractSubtitle(ctx, s, k)
		if err != nil {
			return "", "", err
		}
		return p, "text/vtt; charset=utf-8", nil
	}

	// ── segments ─────────────────────────────────────────────────────────────

	if strings.HasPrefix(name, "seg") && strings.HasSuffix(name, ".ts") {
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "seg"), ".ts"))
		if err != nil {
			return "", "", fmt.Errorf("hls: bad segment %q", name)
		}
		kind := segMuxed
		if multiAudio {
			kind = segVideoOnly
		}
		p, err := m.transcodeSegment(ctx, s, n, kind, 0)
		if err != nil {
			return "", "", err
		}
		return p, "video/mp2t", nil
	}

	// a<k>seg<n>.ts — audio-only segment for stream k.
	// Name format: "a" + k + "seg" + n + ".ts"
	if strings.HasPrefix(name, "a") && strings.HasSuffix(name, ".ts") {
		rest := strings.TrimSuffix(strings.TrimPrefix(name, "a"), ".ts")
		// rest == "<k>seg<n>"
		segIdx := strings.Index(rest, "seg")
		if segIdx < 0 {
			return "", "", fmt.Errorf("hls: bad segment name %q", name)
		}
		k, err := strconv.Atoi(rest[:segIdx])
		if err != nil || k < 0 || k >= len(audioStreams) {
			return "", "", fmt.Errorf("hls: bad audio segment %q", name)
		}
		n, err := strconv.Atoi(rest[segIdx+3:])
		if err != nil {
			return "", "", fmt.Errorf("hls: bad audio segment number in %q", name)
		}
		p, err := m.transcodeSegment(ctx, s, n, segAudioOnly, k)
		if err != nil {
			return "", "", err
		}
		return p, "video/mp2t", nil
	}

	return "", "", fmt.Errorf("hls: not found %q", name)
}

// writePlaylist builds a VOD playlist listing every segment (enables seeking).
// segPrefix is prepended to the segment number in each URI, e.g.:
//
//	segPrefix=""   → seg0.ts, seg1.ts, …  (muxed or video-only)
//	segPrefix="a1" → a1seg0.ts, a1seg1.ts, …  (audio stream 1)
func (s *hlsSession) writePlaylist(path, segPrefix string) error {
	s.mu.RLock()
	dur := s.duration
	s.mu.RUnlock()
	n := 0
	if dur > 0 {
		n = int(math.Ceil(dur / segDur))
	}
	if n == 0 {
		return fmt.Errorf("hls: unknown duration")
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-PLAYLIST-TYPE:VOD\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n", int(segDur)+1)
	for i := 0; i < n; i++ {
		d := segDur
		if i == n-1 {
			if rem := dur - float64(i)*segDur; rem > 0 {
				d = rem
			}
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%sseg%d.ts\n", d, segPrefix, i)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// writeSubPlaylist writes a single-segment VOD subtitle playlist for subtitle
// track k.  The playlist references sub<k>.vtt which is extracted on demand.
// Using a single segment spanning the full duration is correct: subtitle
// parsers handle the full VTT at once, and the player seeks within it natively.
func (s *hlsSession) writeSubPlaylist(path string, k int) error {
	s.mu.RLock()
	dur := s.duration
	s.mu.RUnlock()
	if dur <= 0 {
		dur = 0
	}
	targetDur := int(math.Ceil(dur))
	if targetDur < 1 {
		targetDur = 1
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-PLAYLIST-TYPE:VOD\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n", targetDur)
	fmt.Fprintf(&b, "#EXTINF:%.3f,\nsub%d.vtt\n", dur, k)
	b.WriteString("#EXT-X-ENDLIST\n")
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// extractSubtitle extracts subtitle track k from the session media file to a
// WebVTT file cached in the session directory.  Thread-safe: concurrent calls
// for the same sub<k>.vtt block on the per-filename mutex; the file is written
// atomically (tmp → final rename) so partial writes are never served.
func (m *hlsManager) extractSubtitle(ctx context.Context, s *hlsSession, k int) (string, error) {
	s.mu.RLock()
	sub := s.subtitleStreams[k]
	s.mu.RUnlock()
	filename := fmt.Sprintf("sub%d.vtt", k)
	vttFile := filepath.Join(s.dir, filename)

	l := s.lockFor(filename)
	l.Lock()
	defer l.Unlock()

	if fi, err := os.Stat(vttFile); err == nil && fi.Size() > 0 {
		return vttFile, nil // already extracted
	}

	tmp := vttFile + ".tmp"
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	// -map 0:s:<SubIdx> selects the k-th subtitle stream by its subtitle-stream
	// index (not global index), matching how ffprobe numbers subtitle streams.
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", localize(s.mediaURL),
		"-map", fmt.Sprintf("0:s:%d", sub.SubIdx),
		"-f", "webvtt",
		tmp,
	)
	if err := cmd.Run(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("hls: subtitle extract %s: %w", filename, err)
	}
	if err := os.Rename(tmp, vttFile); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return vttFile, nil
}

// lockFor returns (creating if necessary) the per-filename mutex.
// Keyed by the segment filename so concurrent requests for different streams
// (e.g. "seg0.ts" and "a1seg0.ts") do not block each other.
func (s *hlsSession) lockFor(filename string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.segLocks[filename]
	if !ok {
		l = &sync.Mutex{}
		s.segLocks[filename] = l
	}
	return l
}

// ── transcodeSegment ──────────────────────────────────────────────────────────

// transcodeSegment transcodes segment n on demand (cached to disk in s.dir).
//
//   - segMuxed:     -map 0:v:0? -map 0:a:0?  h264 + aac  (single-audio sessions)
//   - segVideoOnly: -map 0:v:0  -an           h264 only   (multi-audio sessions)
//   - segAudioOnly: -map 0:a:<audioIdx>  -vn  aac only    (multi-audio sessions)
//
// For video kinds (muxed/video-only) the configured hardware encoder is tried
// first; libx264 is the automatic fallback on any error.  Audio is always
// software AAC.
//
// Robustness flags applied to all paths:
//   - Input:  -ss inputSeek (before -i) + -ss outputSeek (after -i) = hybrid seek
//     -fflags +genpts -analyzeduration 2000000 -probesize 2000000
//   - Video:  GOP alignment (-g/-keyint_min), bitrate caps (-b:v/-maxrate/-bufsize)
//   - Audio:  -af aresample=async=1:first_pts=0,apad  (keeps A/V aligned on seek)
//   - Output: -output_ts_offset -muxdelay 0 -t dur -mpegts_copyts 1
//     (-t as output option terminates apad in audio-only segments)
//
// High-bit-depth safety: we never add -hwaccel decode flags (SW decode is always
// used, which is safe across all input formats).  For VAAPI the filter chain
// "format=nv12,hwupload" handles 10→8 bit downconversion before GPU upload.
// For other HW encoders, an explicit "format=yuv420p" filter is prepended when
// s.highBitDepth is true.
func (m *hlsManager) transcodeSegment(ctx context.Context, s *hlsSession, n int, kind segKind, audioIdx int) (string, error) {
	s.mu.RLock()
	sessionDur := s.duration
	highBitDepth := s.highBitDepth
	s.mu.RUnlock()
	var filename string
	if kind == segAudioOnly {
		filename = fmt.Sprintf("a%dseg%d.ts", audioIdx, n)
	} else {
		filename = fmt.Sprintf("seg%d.ts", n)
	}
	segFile := filepath.Join(s.dir, filename)

	l := s.lockFor(filename)
	l.Lock()
	defer l.Unlock()
	if fi, err := os.Stat(segFile); err == nil && fi.Size() > 0 {
		return segFile, nil // already transcoded; serve from cache
	}

	// Bound concurrent ffmpeg spawns: a client prefetch burst must not fork
	// unbounded processes.  Respect the caller context so a disconnected client
	// releases the slot rather than blocking indefinitely.
	select {
	case m.transcodeSem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-m.transcodeSem }()

	start := float64(n) * segDur
	dur := segDur
	if sessionDur > 0 {
		if rem := sessionDur - start; rem > 0 && rem < dur {
			dur = rem
		}
	}
	tmp := segFile + ".tmp.ts"

	gopStr := strconv.Itoa(videoGOP)

	// Hybrid seeking: fast keyframe seek to (start-10s) before -i, then
	// accurate output seek for the residual after -i.  This is much faster
	// than pure output seeking for mid-file segments while still landing on the
	// correct frame.  The 10-second safety margin ensures we always decode from
	// a keyframe before the target and the output -ss discards the gap.
	inputSeek := math.Max(0, start-10.0)
	outputSeek := start - inputSeek

	// run builds the full ffmpeg argument list for the given encoder and executes
	// the transcode.  enc is either m.enc (hardware attempt) or the libx264
	// fallback.  The function is called at most twice: HW first, SW on error.
	run := func(enc hwEncoder) error {
		a := []string{"-hide_banner", "-loglevel", "error", "-y"}

		// Input robustness: re-generate missing timestamps; cap probe overhead.
		a = append(a, "-fflags", "+genpts",
			"-analyzeduration", "2000000", "-probesize", "2000000")

		// Pre-input device flags (VAAPI: global -vaapi_device must precede -i).
		// We never add -hwaccel / -hwaccel_device flags: SW decode is always used
		// (avoids driver compatibility issues with unusual input codecs/formats).
		if enc.codec == "h264_vaapi" && enc.driDevice != "" {
			a = append(a, "-vaapi_device", enc.driDevice)
		}

		// Hybrid seek: coarse input seek (keyframe-aligned) before -i, then
		// fine output seek for the residual after -i.
		if inputSeek > 0 {
			a = append(a, "-ss", ftoa(inputSeek))
		}
		a = append(a, "-i", localize(s.mediaURL))
		// Output seek: precise, decodes from inputSeek and discards until start.
		if outputSeek > 0 {
			a = append(a, "-ss", ftoa(outputSeek))
		}

		// Stream mapping.
		switch kind {
		case segMuxed:
			// ? makes each stream optional so video-only files don't error.
			a = append(a, "-map", "0:v:0?", "-map", "0:a:0?")
		case segVideoOnly:
			a = append(a, "-map", "0:v:0", "-an")
		case segAudioOnly:
			// audioIdx is 0-based among audio streams; 0:a:k selects the k-th audio.
			a = append(a, "-map", fmt.Sprintf("0:a:%d", audioIdx), "-vn")
		}

		// ── Video encoding (segMuxed and segVideoOnly) ────────────────────────
		if kind != segAudioOnly {
			switch enc.codec {
			case "h264_vaapi":
				// SW decode + GPU encode: robust across codecs and bit-depths.
				// format=nv12 converts 10/12-bit → 8-bit NV12 before hwupload;
				// this is why we do not need explicit -pix_fmt for high-bit-depth.
				a = append(a,
					"-vf", "format=nv12,hwupload",
					"-c:v", "h264_vaapi", "-qp", "23",
					"-g", gopStr,
					"-b:v", videoBitrateCap, "-maxrate", videoMaxrate, "-bufsize", videoBufsize,
				)

			case "h264_nvenc":
				// 10-bit inputs crash NVENC without explicit format conversion.
				if highBitDepth {
					a = append(a, "-vf", "format=yuv420p")
				}
				a = append(a,
					"-c:v", "h264_nvenc", "-preset", "p4", "-rc", "vbr",
					"-g", gopStr,
					"-b:v", videoBitrateCap, "-maxrate", videoMaxrate, "-bufsize", videoBufsize,
				)

			case "h264_qsv":
				if highBitDepth {
					a = append(a, "-vf", "format=yuv420p")
				}
				a = append(a,
					"-c:v", "h264_qsv", "-preset", "veryfast",
					"-g", gopStr,
					"-b:v", videoBitrateCap, "-maxrate", videoMaxrate, "-bufsize", videoBufsize,
				)

			case "h264_videotoolbox":
				if highBitDepth {
					a = append(a, "-vf", "format=yuv420p")
				}
				a = append(a,
					"-c:v", "h264_videotoolbox", "-realtime", "1",
					"-g", gopStr,
					"-b:v", videoBitrateCap, "-maxrate", videoMaxrate, "-bufsize", videoBufsize,
				)

			case "h264_v4l2m2m":
				if highBitDepth {
					a = append(a, "-vf", "format=yuv420p")
				}
				a = append(a,
					"-c:v", "h264_v4l2m2m",
					"-g", gopStr,
					"-b:v", videoBitrateCap, "-maxrate", videoMaxrate, "-bufsize", videoBufsize,
				)

			default: // libx264 — quality-constrained CRF with bitrate ceiling
				a = append(a,
					"-vf", "format=yuv420p", // ensure 8-bit even for 10-bit inputs
					"-c:v", "libx264", "-preset", "veryfast", "-crf", "23",
					"-profile:v", "high",
					// sc_threshold=0 disables scene-cut detection so -g is strictly
					// honoured; -keyint_min enforces IDR at every GOP boundary.
					"-sc_threshold", "0",
					"-g", gopStr, "-keyint_min", gopStr,
					"-b:v", videoBitrateCap, "-maxrate", videoMaxrate, "-bufsize", videoBufsize,
				)
			}
		}

		// ── Audio encoding (segMuxed and segAudioOnly) ────────────────────────
		// aresample=async=1:first_pts=0 realigns audio to presentation timestamps
		// after a seek; apad pads short final segments to prevent under-runs.
		switch kind {
		case segMuxed:
			a = append(a,
				"-c:a", "aac", "-ac", "2", "-b:a", "192k",
				"-af", "aresample=async=1:first_pts=0,apad",
				"-sn", // drop subtitle streams from the output
			)
		case segAudioOnly:
			a = append(a,
				"-c:a", "aac", "-ac", "2", "-b:a", "192k",
				"-af", "aresample=async=1:first_pts=0,apad",
			)
		}

		// ── Output mux ────────────────────────────────────────────────────────
		// -output_ts_offset: set the PTS/DTS of the first packet to its position
		//   in the full timeline so the player does not restart at 0.
		// -muxdelay 0:       suppress mpegts muxer buffering jitter.
		// -mpegts_copyts 1:  preserve codec timestamps verbatim in the container.
		// -t dur (output):   CRITICAL for audio paths — terminates the apad filter
		//   which would otherwise pad indefinitely in audio-only segments (no video
		//   reference to signal EOF). Also acts as a safety ceiling for all paths.
		a = append(a,
			"-output_ts_offset", ftoa(start),
			"-muxdelay", "0",
			"-t", ftoa(dur),
			"-mpegts_copyts", "1",
			"-f", "mpegts", tmp,
		)

		ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, "ffmpeg", a...).Run()
	}

	// Attempt hardware encode; fall back to libx264 on any error.
	// If the caller's context is already done when the HW attempt fails,
	// propagate the cancellation directly — do not start a software re-encode
	// for a segment that is no longer needed.
	sw := hwEncoder{codec: "libx264"}
	var err error
	if m.enc.isHW && kind != segAudioOnly {
		// VAAPI/NVENC/etc. accelerates h264 video encoding only; audio is always SW.
		if err = run(m.enc); err != nil {
			_ = os.Remove(tmp)
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			err = run(sw) // transparent software fallback
		}
	} else {
		err = run(sw)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("hls: transcode %s: %w", filename, err)
	}
	if err := os.Rename(tmp, segFile); err != nil {
		return "", err
	}
	return segFile, nil
}

// ── CloseHLS ──────────────────────────────────────────────────────────────────

// CloseHLS stops the background reaper and removes all session working directories.
// Safe to call multiple times.
func (m *hlsManager) CloseHLS() {
	// Signal the reaper to stop; guard against double-close with a non-blocking
	// drain: if stopCh is already closed the receive arm fires immediately.
	select {
	case <-m.stopCh:
	default:
		close(m.stopCh)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		_ = os.RemoveAll(s.dir)
	}
	m.sessions = map[string]*hlsSession{}
}

// reaper is the single background goroutine that evicts idle HLS sessions.
// It runs until CloseHLS closes stopCh, so it never leaks.
func (m *hlsManager) reaper() {
	ticker := time.NewTicker(reaperInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.evictIdle()
		case <-m.stopCh:
			return
		}
	}
}

// evictIdle removes sessions whose lastAccess timestamp is older than sessionTTL.
// Deleting from a map during range is safe and defined in Go.
func (m *hlsManager) evictIdle() {
	cutoff := time.Now().Add(-sessionTTL)
	m.mu.Lock()
	for id, s := range m.sessions {
		ts := s.lastAccess.Load()
		if ts == 0 {
			// Session created but not yet accessed (e.g. in the window between
			// map insertion and the first lastAccess.Store); skip to be safe.
			continue
		}
		if time.Unix(0, ts).Before(cutoff) {
			delete(m.sessions, id)
			_ = os.RemoveAll(s.dir)
		}
	}
	m.mu.Unlock()
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', 3, 64) }

// --- types.MediaProber HLS methods (delegate to the manager) ---

func (p *prober) StartHLS(id, mediaURL string) (string, error) { return p.hls.StartHLS(id, mediaURL) }
func (p *prober) HLSFile(ctx context.Context, id, name string) (string, string, error) {
	return p.hls.HLSFile(ctx, id, name)
}

// CloseHLS stops the background session reaper and removes all HLS working
// directories.  Not part of types.MediaProber; call directly on shutdown.
func (p *prober) CloseHLS() { p.hls.CloseHLS() }

// Sessions returns the number of currently active HLS transcode sessions.
func (m *hlsManager) Sessions() int {
	m.mu.Lock()
	n := len(m.sessions)
	m.mu.Unlock()
	return n
}

// HLSSessions returns the number of active HLS transcode sessions.
// Satisfies the interface checked by handleMetrics via structural assertion.
func (p *prober) HLSSessions() int { return p.hls.Sessions() }
