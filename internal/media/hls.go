package media

import (
	"context"
	"encoding/json"
	"fmt"
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

	"github.com/M0Rf30/stremio-server-go/internal/logging"
)

// segDur is the HLS segment length in seconds.
const segDur = 4.0

// videoGOP is the fixed keyframe interval for all video encodes.
// Computed as round(30fps × 4s) = 120.  Aligns segment boundaries with IDR
// frames so the player can seek without decoding a preceding GOP.
const videoGOP = 120

// segKind identifies what streams a transcoded segment contains.
type segKind int

const (
	segMuxed     segKind = iota // video + first-audio, muxed (single-audio session)
	segVideoOnly                // video only, no audio (multi-audio session)
	segAudioOnly                // audio only for one track (multi-audio session)
)

// probeCacheMaxSize is the maximum number of entries kept in the negative
// probe cache.  Expired entries are swept on every insert; when the map
// still exceeds this limit the soonest-expiring entry is evicted.
const probeCacheMaxSize = 512

// maxSegIdx is the hard upper bound on any caller-supplied segment index.
// 1<<18 == 262 144 segments × 4 s ≈ 12 days; far beyond any real media.
const maxSegIdx = 1 << 18

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
	srcWidth        int                    // probed video width; 0 if unknown/no video stream
	srcHeight       int                    // probed video height; 0 if unknown/no video stream
	segLocks        map[string]*sync.Mutex // keyed by segment filename
	lastAccess      atomic.Int64           // unix nanoseconds; updated on each StartHLS/HLSFile call
	// inFlight counts calls currently executing HLSFile (which covers
	// transcodeSegment/extractSubtitle/writePlaylist) for this session.
	// evictIdle refuses to remove any session with inFlight > 0, so the
	// reaper can never os.RemoveAll(s.dir) while ffmpeg is still writing
	// seg<n>.ts.tmp.ts into it. Incremented/decremented with defer so it
	// cannot leak on any error path.
	inFlight atomic.Int32
	// playlistData records which segPrefix playlists have already been rendered
	// and written to disk; content is immutable once duration is set, so a
	// presence marker is enough to skip the rebuild + write.  Guarded by mu.
	playlistData map[string]struct{}
	// tc is a snapshot of the effective transcode configuration taken once,
	// at session creation (StartHLS), applying any live /settings overrides
	// active at that moment (issue #20). Immutable for the life of the
	// session so every segment is encoded consistently even if /settings
	// changes again mid-playback; a new session picks up the new values.
	tc sessionConfig
}

// hwEncoder holds the selected H.264 encoder identity plus any device path
// required for it.  Software fallback is codec="libx264", isHW=false.
type hwEncoder struct {
	codec     string // ffmpeg codec name: "h264_vaapi", "h264_nvenc", "libx264", …
	isHW      bool   // false only for libx264
	driDevice string // VAAPI renderD* path; empty for all non-VAAPI codecs
}

// probeCacheEntry records a cached ffprobe result (positive or negative).
// Negative (zero-duration/error) results use HLSConfig.NegProbeTTL; positive
// results use HLSConfig.PosProbeTTL so duplicate sessions for the same URL
// skip re-probing.
type probeCacheEntry struct {
	result    probeMediaResult
	expiresAt time.Time
}

// hlsManager owns per-id sessions and the hardware-accel decision.
// enc is set once in newHLS() and is read-only thereafter; cfg is likewise
// resolved once (env-derived) and read-only — per-session /settings
// overrides live on hlsSession.tc instead (see effectiveSessionConfig).
type hlsManager struct {
	base string
	// selfBase is this server's own local base URL; media URLs pointing at it
	// are exempt from the private-address SSRF check (see validateRemoteURL).
	selfBase string
	enc      hwEncoder
	cfg      HLSConfig
	// settings is consulted at session-creation time (StartHLS) and on every
	// transcode-slot acquisition to apply live /settings overrides (issue
	// #20 "Related"). May be nil (no /settings integration; env defaults
	// only), which every settings* helper treats as "no override".
	settings SettingsSource

	mu         sync.Mutex
	sessions   map[string]*hlsSession
	probeCache map[string]probeCacheEntry // probe cache (positive+negative); keyed by mediaURL
	// semHeld is the number of concurrent ffmpeg segment-transcode slots
	// currently held, bounded by currentConcurrency() (see
	// acquireTranscodeSlot). A plain atomic counter instead of a
	// fixed-capacity channel so the limit can change live via /settings
	// without rebuilding the primitive.
	semHeld atomic.Int32
	stopCh  chan struct{} // closed by CloseHLS to stop the reaper
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
// actually works on this machine.  Called once in newHLS(). driDevice is the
// VAAPI render node to use (HLSConfig.VAAPIDevice; STREMIO_HLS_VAAPI_DEVICE,
// default "/dev/dri/renderD128") — configurable for multi-GPU hosts (issue
// #20).
//
// Selection rules:
//   - STREMIO_HWACCEL=0                            → always libx264 (hard switch)
//   - STREMIO_HWACCEL=nvenc|qsv|vaapi|…            → explicit; listed encoder used
//     without the verify step (user accepts responsibility)
//   - STREMIO_HWACCEL="" or "auto" (default)       → auto; only verified encoders
//     chosen; priority: NVENC > QSV > VideoToolbox > VAAPI > V4L2M2M > libx264
func selectEncoder(driDevice string) hwEncoder {
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
			return hwEncoder{codec: "h264_vaapi", isHW: true, driDevice: driDevice}
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
	if listed("h264_vaapi") {
		if _, statErr := os.Stat(driDevice); statErr == nil {
			preIn := []string{"-vaapi_device", driDevice}
			preEnc := []string{"-vf", "format=nv12,hwupload"}
			if verifyEncoder("h264_vaapi", preIn, preEnc) {
				return hwEncoder{codec: "h264_vaapi", isHW: true, driDevice: driDevice}
			}
		}
	}
	// V4L2 Memory-to-Memory (ARM / Raspberry Pi)
	if listed("h264_v4l2m2m") && verifyEncoder("h264_v4l2m2m", nil, nil) {
		return hwEncoder{codec: "h264_v4l2m2m", isHW: true}
	}

	return sw // no working hardware encoder found
}

// newHLS constructs the manager for one server instance. cfg is normalized
// (zero-value fields resolved to defaults, SegmentConcurrency resolved to
// runtime.NumCPU() when unset) before use; settings is consulted for live
// /settings overrides at session-creation time (may be nil).
func newHLS(selfBase string, cfg HLSConfig, settings SettingsSource) *hlsManager {
	cfg = cfg.normalize(runtime.NumCPU())
	base := newHLSBaseDir(cfg.WorkDir)
	enc := selectEncoder(cfg.VAAPIDevice)
	if enc.isHW {
		logging.For("media").Info("HLS transcode using hardware encoder", "encoder", enc.codec, "device", enc.driDevice)
	} else {
		logging.For("media").Warn("HLS transcode using software encoder; expect high CPU", "encoder", enc.codec, "hint", "set STREMIO_HWACCEL=vaapi and ensure /dev/dri access")
	}
	m := &hlsManager{
		base:       base,
		selfBase:   selfBase,
		enc:        enc,
		cfg:        cfg,
		settings:   settings,
		sessions:   map[string]*hlsSession{},
		probeCache: map[string]probeCacheEntry{},
		stopCh:     make(chan struct{}),
	}
	go m.reaper()
	return m
}

// newHLSBaseDir creates and returns a fresh, unpredictable, owner-only
// (0700) working directory for one hlsManager instance (MED-2).
//
// workDir == "" preserves the original behaviour: os.MkdirTemp gives each
// manager instance a fresh, unpredictable, 0700 (owner-only) directory
// instead of a fixed, shared, 0755 path — which a co-resident user on a
// multi-user host could pre-create (or read) before this process ever
// started. workDir != "" (HLSConfig.WorkDir / STREMIO_HLS_WORK_DIR, issue
// #20) creates the same kind of fresh stremio-hls-* directory, rooted under
// workDir instead of the OS default temp dir — e.g. to place HLS segments on
// a specific fast or large disk. Falls back to the workDir-less behaviour if
// workDir can't be created/used.
func newHLSBaseDir(workDir string) string {
	if workDir != "" {
		if err := os.MkdirAll(workDir, 0o700); err == nil {
			if base, mkErr := os.MkdirTemp(workDir, "stremio-hls-*"); mkErr == nil {
				return base
			}
		}
		logging.For("media").Warn("STREMIO_HLS_WORK_DIR unusable; falling back to the OS temp dir", "work_dir", workDir)
	}
	base, err := os.MkdirTemp("", "stremio-hls-*")
	if err != nil {
		// Fall back to a per-process-unique path rather than the old
		// predictable shared name if MkdirTemp itself fails (e.g. a
		// read-only default temp dir).
		base = filepath.Join(os.TempDir(), fmt.Sprintf("stremio-hls-%d", os.Getpid()))
		_ = os.MkdirAll(base, 0o700)
	}
	return base
}

// localize rewrites the self-signed https loopback URL to plain http so ffmpeg
// (which doesn't ignore TLS errors) can read the stream.
func localize(u string) string {
	return strings.ReplaceAll(u, "https://127.0.0.1:12470", "http://127.0.0.1:11470")
}

// sanitizeM3U8Attr removes characters that are illegal inside a quoted-string
// M3U8 attribute value: double-quote, carriage return, newline, and comma.
// These would break the playlist syntax; stripping them is safe because the
// values are only decorative labels (NAME, LANGUAGE).
func sanitizeM3U8Attr(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '"', '\r', '\n', ',':
			return -1
		}
		return r
	}, s)
}

// ── probeMedia ────────────────────────────────────────────────────────────────

// probeMediaResult carries the output of a combined ffprobe run.
type probeMediaResult struct {
	duration        float64
	audioStreams    []audioStream
	subtitleStreams []subtitleStream
	highBitDepth    bool // true if any video stream is 10/12-bit
	width           int  // first video stream's width; 0 if unknown/no video stream
	height          int  // first video stream's height; 0 if unknown/no video stream
}

// probeMedia runs a single ffprobe with -show_format -show_streams and returns
// the media duration, every audio stream, whether the video is high-bit-depth,
// and the first video stream's resolution. timeout bounds the ffprobe child
// process (HLSConfig.ProbeTimeout; STREMIO_HLS_PROBE_TIMEOUT, default 30s —
// issue #20: slow-starting torrents/debrid links can exceed the historical
// fixed 30s). mediaURL is routed through the loopback relay (SEC-4) unless it
// matches selfBase. The caller must NOT hold any session lock when calling
// this function.
func probeMedia(ctx context.Context, mediaURL, selfBase string, timeout time.Duration) probeMediaResult {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	inputURL, protoWL, err := relayInput(mediaURL, selfBase)
	if err != nil {
		return probeMediaResult{}
	}
	out, err := runCapped(exec.CommandContext(ctx, "ffprobe",
		"-v", "quiet", "-print_format", "json",
		"-protocol_whitelist", protoWL,
		"-show_format", "-show_streams",
		inputURL), ffprobeOutputLimit) // MED-1: bound ffprobe stdout
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
			Width     int    `json:"width"`
			Height    int    `json:"height"`
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
	var width, height int
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
			// Use the first video stream's dimensions (embedded thumbnails/
			// attached-pic streams, if any, are additional video streams).
			if width == 0 && height == 0 {
				width, height = st.Width, st.Height
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
	return probeMediaResult{
		duration: d, audioStreams: audio, subtitleStreams: subs, highBitDepth: highBit,
		width: width, height: height,
	}
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
//
// The master playlist's BANDWIDTH/CODECS are derived from the effective
// bitrate cap and output resolution (issue #20) — see deriveBandwidthCodecs.
func (m *hlsManager) StartHLS(id, mediaURL string) (string, error) {
	if mediaURL == "" {
		return "", fmt.Errorf("hls: missing mediaURL")
	}
	// Validated once here, before mediaURL is ever stored on the session, so
	// every later ffprobe/ffmpeg call on s.mediaURL (probeMedia, extractSubtitle,
	// transcodeSegment) is guaranteed to already be an http(s) URL on a
	// non-private, non-metadata host — or this server's own origin, which is how
	// the https UI on :12470 asks for a stream this server serves on :11470.
	if err := validateRemoteURL(mediaURL, m.selfBase); err != nil {
		return "", fmt.Errorf("hls: %w", err)
	}
	// Reject ids that could escape the base directory via path traversal.
	if id == "" || id == "." || id == ".." || id != filepath.Base(id) || strings.Contains(id, "..") {
		return "", fmt.Errorf("hls: invalid session id %q", id)
	}
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok {
		if len(m.sessions) >= m.cfg.MaxSessions {
			m.mu.Unlock()
			return "", fmt.Errorf("hls: too many concurrent sessions (limit %d)", m.cfg.MaxSessions)
		}
		s = &hlsSession{
			mediaURL:     mediaURL,
			dir:          filepath.Join(m.base, id),
			segLocks:     map[string]*sync.Mutex{},
			playlistData: map[string]struct{}{},
			// Snapshot the effective transcode config now, at session
			// creation, so a POST /settings change afterward only affects
			// the *next* new session — see hlsSession.tc's doc comment.
			tc: m.effectiveSessionConfig(),
		}
		s.lastAccess.Store(time.Now().UnixNano())
		_ = os.MkdirAll(s.dir, 0o755)
		m.sessions[id] = s
	}
	m.mu.Unlock()

	// Refresh lastAccess for both new and reused sessions.
	s.lastAccess.Store(time.Now().UnixNano())

	// Probe outside the session write lock so a slow ffprobe does not stall
	// concurrent HLSFile calls on the same session.
	s.mu.RLock()
	needProbe := s.duration == 0
	s.mu.RUnlock()

	if needProbe {
		// Negative-probe cache: avoid hammering a broken URL with repeated probes.
		m.mu.Lock()
		cached, hasCached := m.probeCache[mediaURL]
		m.mu.Unlock()

		var res probeMediaResult
		if hasCached && time.Now().Before(cached.expiresAt) {
			res = cached.result
		} else {
			res = probeMedia(context.Background(), mediaURL, m.selfBase, m.cfg.ProbeTimeout)
			m.mu.Lock()
			if res.duration == 0 {
				// Cache the negative result to short-circuit future probes.
				m.probeCache[mediaURL] = probeCacheEntry{
					result:    res,
					expiresAt: time.Now().Add(m.cfg.NegProbeTTL),
				}
				// Prune expired / excess entries to bound cache size.
				m.sweepProbeCache()
			} else {
				// Cache positive result; subsequent sessions for the same URL
				// skip the expensive ffprobe entirely until it expires.
				// sweepProbeCache evicts expired/excess entries as usual.
				m.probeCache[mediaURL] = probeCacheEntry{
					result:    res,
					expiresAt: time.Now().Add(m.cfg.PosProbeTTL),
				}
				m.sweepProbeCache()
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
			s.srcWidth = res.width
			s.srcHeight = res.height
		}
		s.mu.Unlock()
	}

	s.mu.RLock()
	multiAudio := s.multiAudio
	audioStreams := s.audioStreams
	subtitleStreams := s.subtitleStreams
	srcWidth, srcHeight := s.srcWidth, s.srcHeight
	tc := s.tc
	s.mu.RUnlock()

	outW, outH, scaled := computeScaledDims(srcWidth, srcHeight, tc.maxWidth, tc.maxHeight)
	bandwidth, codecsVideo := deriveBandwidthCodecs(tc, outW, outH, scaled)

	return buildMasterPlaylist(masterPlaylistInputs{
		multiAudio:      multiAudio,
		audioStreams:    audioStreams,
		subtitleStreams: subtitleStreams,
		bandwidth:       bandwidth,
		codecsVideo:     codecsVideo,
	}), nil
}

// masterPlaylistInputs bundles everything buildMasterPlaylist needs,
// decoupled from hlsSession/probing so the playlist text can be
// golden-tested without a real ffprobe/ffmpeg session.
type masterPlaylistInputs struct {
	multiAudio      bool
	audioStreams    []audioStream
	subtitleStreams []subtitleStream
	bandwidth       int64
	codecsVideo     string // H.264 profile-level CODECS token, e.g. "avc1.640029"
}

// buildMasterPlaylist renders the HLS master playlist text for one session.
// See StartHLS's doc comment for the produced variant shapes; bandwidth/
// codecsVideo come from deriveBandwidthCodecs.
func buildMasterPlaylist(in masterPlaylistInputs) string {
	hasSubs := len(in.subtitleStreams) > 0
	streamInf := fmt.Sprintf("BANDWIDTH=%d,CODECS=\"%s,mp4a.40.2\"", in.bandwidth, in.codecsVideo)

	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:4\n")

	// ── Subtitle rendition group ──────────────────────────────────────────────
	// One EXT-X-MEDIA:TYPE=SUBTITLES entry per text subtitle stream.
	// The GROUP-ID "subs" is referenced in every EXT-X-STREAM-INF below.
	if hasSubs {
		for k, sub := range in.subtitleStreams {
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
				sanitizeM3U8Attr(lang), sanitizeM3U8Attr(name), isDefault, k,
			)
		}
	}

	if !in.multiAudio {
		// Single-audio (or no-audio) path.
		if hasSubs {
			fmt.Fprintf(&b, "#EXT-X-STREAM-INF:%s,SUBTITLES=\"subs\"\nplaylist.m3u8\n", streamInf)
		} else {
			fmt.Fprintf(&b, "#EXT-X-STREAM-INF:%s\nplaylist.m3u8\n", streamInf)
		}
		return b.String()
	}

	// ── Multi-audio master playlist ───────────────────────────────────────────
	// Player audio-menu behaviour: the browser/AVPlayer sees each EXT-X-MEDIA
	// entry as a selectable audio rendition.  It follows the URI to audio<k>.m3u8
	// and fetches the corresponding a<k>seg<n>.ts files.

	// Determine which stream gets DEFAULT=YES (first with disposition.default,
	// or stream 0 if none is marked default).
	defaultIdx := 0
	for k, a := range in.audioStreams {
		if a.IsDefault {
			defaultIdx = k
			break
		}
	}

	for k, a := range in.audioStreams {
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
			langAttr = fmt.Sprintf(",LANGUAGE=\"%s\"", sanitizeM3U8Attr(a.Language))
		}
		fmt.Fprintf(&b,
			"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\"%s,NAME=\"%s\",DEFAULT=%s,AUTOSELECT=%s,URI=\"audio%d.m3u8\"\n",
			langAttr, sanitizeM3U8Attr(name), defaultVal, autoVal, k,
		)
	}

	// Single video-only variant; AUDIO="aud" links it to the audio rendition group.
	// SUBTITLES="subs" links it to the subtitle rendition group when subs are present.
	if hasSubs {
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:%s,AUDIO=\"aud\",SUBTITLES=\"subs\"\nvideo.m3u8\n", streamInf)
	} else {
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:%s,AUDIO=\"aud\"\nvideo.m3u8\n", streamInf)
	}
	return b.String()
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
	if ok {
		// Touch lastAccess and mark work in flight while still holding m.mu so
		// the reaper cannot evict this session between the map lookup and the
		// update (see evictIdle's inFlight check).
		s.lastAccess.Store(time.Now().UnixNano())
		s.inFlight.Add(1)
	}
	m.mu.Unlock()
	if !ok {
		return "", "", fmt.Errorf("hls: unknown session %s", id)
	}
	// Held for the whole call, including transcodeSegment/extractSubtitle,
	// which can run well past SessionTTL on a slow/software-encode host.
	// evictIdle skips any session with inFlight > 0, so a concurrent reaper
	// pass can never os.RemoveAll(s.dir) out from under an in-progress ffmpeg
	// write. lastAccess is refreshed again on the way out (success or error)
	// so the idle clock restarts from completion, not from before the
	// transcode began.
	defer func() {
		s.lastAccess.Store(time.Now().UnixNano())
		s.inFlight.Add(-1)
	}()

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
	sessionDur := s.duration
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
		if err != nil || n < 0 || n > maxSegIdx {
			return "", "", fmt.Errorf("hls: bad segment %q", name)
		}
		if sessionDur > 0 {
			if n >= int(math.Ceil(sessionDur/segDur)) {
				return "", "", fmt.Errorf("hls: segment %d beyond media duration (%.0fs)", n, sessionDur)
			}
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
		if err != nil || n < 0 || n > maxSegIdx {
			return "", "", fmt.Errorf("hls: bad audio segment number in %q", name)
		}
		if sessionDur > 0 {
			if n >= int(math.Ceil(sessionDur/segDur)) {
				return "", "", fmt.Errorf("hls: segment %d beyond media duration (%.0fs)", n, sessionDur)
			}
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
	// Check cache under read-lock; playlist bytes are immutable once duration is set.
	s.mu.RLock()
	_, ok := s.playlistData[segPrefix]
	dur := s.duration
	s.mu.RUnlock()
	if ok {
		// Already rendered and written to disk on a prior request — skip rebuild.
		return nil
	}
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
	data := []byte(b.String())
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	// Store rendered bytes so future requests for the same segPrefix skip the
	// O(n_segments) format loop and disk write entirely.
	s.mu.Lock()
	if s.playlistData == nil {
		s.playlistData = make(map[string]struct{})
	}
	s.playlistData[segPrefix] = struct{}{}
	s.mu.Unlock()
	return nil
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
	ctx, cancel := context.WithTimeout(ctx, m.cfg.SubtitleTimeout)
	defer cancel()
	// SEC-4: route through the loopback relay instead of handing ffmpeg the
	// real remote URL directly.
	inputURL, protoWL, rerr := relayInput(s.mediaURL, m.selfBase)
	if rerr != nil {
		return "", fmt.Errorf("hls: subtitle extract %s: %w", filename, rerr)
	}
	// -map 0:s:<SubIdx> selects the k-th subtitle stream by its subtitle-stream
	// index (not global index), matching how ffprobe numbers subtitle streams.
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-protocol_whitelist", protoWL,
		"-i", inputURL,
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

// ── transcode concurrency ───────────────────────────────────────────────────

// acquireTranscodeSlot blocks until a concurrent-transcode slot is available
// or ctx is done. Unlike a fixed-capacity channel, currentConcurrency() is
// re-read on every attempt, so a live POST /settings transcodeConcurrency
// change takes effect for the very next queued transcode — concurrency
// bounds a single process-wide resource shared by every session, so
// "session creation time" (used by the other /settings-overridable knobs)
// isn't a natural fit here.
func (m *hlsManager) acquireTranscodeSlot(ctx context.Context) error {
	for {
		limit := int32(m.currentConcurrency())
		cur := m.semHeld.Load()
		if cur < limit && m.semHeld.CompareAndSwap(cur, cur+1) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// releaseTranscodeSlot releases a slot acquired by acquireTranscodeSlot.
func (m *hlsManager) releaseTranscodeSlot() {
	m.semHeld.Add(-1)
}

// currentConcurrency resolves the live effective SegmentConcurrency: the
// env-configured default (m.cfg.SegmentConcurrency, resolved to
// runtime.NumCPU() at construction when unset) unless the live
// transcodeConcurrency /settings value is set to something other than its
// settings-schema default of 1 (see effectiveSessionConfig's doc comment for
// why the schema default is excluded rather than treated as an active
// override).
func (m *hlsManager) currentConcurrency() int {
	limit := m.cfg.SegmentConcurrency
	if v, ok := settingsInt(m.settings, "transcodeConcurrency"); ok && v > 0 && v != 1 {
		limit = v
	}
	if limit < 1 {
		limit = 1
	}
	return limit
}

// ── transcodeSegment ──────────────────────────────────────────────────────────

// transcodeSegment transcodes segment n on demand (cached to disk in s.dir).
//
//   - segMuxed:     -map 0:v:0? -map 0:a:0?  h264 + aac  (single-audio sessions)
//   - segVideoOnly: -map 0:v:0  -an           h264 only   (multi-audio sessions)
//   - segAudioOnly: -map 0:a:<audioIdx>  -vn  aac only    (multi-audio sessions)
//
// For video kinds (muxed/video-only) the configured hardware encoder is tried
// first (unless the session's tc.hwEnabled is false — transcodeHardwareAccel
// /settings override, issue #20); libx264 is the automatic fallback on any
// error. Audio is always software AAC.
//
// Robustness flags applied to all paths:
//   - Input:  -ss inputSeek (before -i) + -ss outputSeek (after -i) = hybrid seek
//     -fflags +genpts -analyzeduration 2000000 -probesize 2000000
//   - Video:  GOP alignment (-g/-keyint_min), bitrate caps (-b:v/-maxrate/-bufsize,
//     session-effective — see hlsSession.tc), optional downscale (scale/scale_vaapi,
//     see buildVideoFilter)
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
	srcWidth, srcHeight := s.srcWidth, s.srcHeight
	tc := s.tc
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
	if err := m.acquireTranscodeSlot(ctx); err != nil {
		return "", err
	}
	defer m.releaseTranscodeSlot()

	start := float64(n) * segDur
	dur := segDur
	if sessionDur > 0 {
		if rem := sessionDur - start; rem > 0 && rem < dur {
			dur = rem
		}
	}
	tmp := segFile + ".tmp.ts"

	gopStr := strconv.Itoa(videoGOP)

	// Downscale target for this segment's video (no-op unless MaxWidth/
	// MaxHeight is configured and the source exceeds it — see
	// computeScaledDims: aspect-preserving, even dimensions, never upscales).
	outW, outH, scaled := computeScaledDims(srcWidth, srcHeight, tc.maxWidth, tc.maxHeight)

	// Hybrid seeking: fast keyframe seek to (start-10s) before -i, then
	// accurate output seek for the residual after -i.  This is much faster
	// than pure output seeking for mid-file segments while still landing on the
	// correct frame.  The 10-second safety margin ensures we always decode from
	// a keyframe before the target and the output -ss discards the gap.
	inputSeek := math.Max(0, start-10.0)
	outputSeek := start - inputSeek

	// SEC-4: route through the loopback relay instead of handing ffmpeg the
	// real remote URL directly. Computed once and reused by both the
	// hardware and (on fallback) software encode attempts below.
	inputURL, protoWL, rerr := relayInput(s.mediaURL, m.selfBase)
	if rerr != nil {
		return "", fmt.Errorf("hls: transcode %s: %w", filename, rerr)
	}

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
		a = append(a, "-protocol_whitelist", protoWL, "-i", inputURL)
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
				// scale_vaapi (when scaled) runs after hwupload, on the GPU frame.
				a = append(a,
					"-vf", buildVideoFilter(enc.codec, true, outW, outH, scaled),
					"-c:v", "h264_vaapi", "-qp", strconv.Itoa(m.cfg.VAAPIQP),
					"-g", gopStr,
					"-b:v", tc.videoBitrate, "-maxrate", tc.videoMaxrate, "-bufsize", tc.videoBufsize,
				)

			case "h264_nvenc":
				// 10-bit inputs crash NVENC without explicit format conversion.
				if vf := buildVideoFilter(enc.codec, highBitDepth, outW, outH, scaled); vf != "" {
					a = append(a, "-vf", vf)
				}
				a = append(a,
					"-c:v", "h264_nvenc", "-preset", tc.nvencPreset, "-rc", "vbr",
					"-g", gopStr,
					"-b:v", tc.videoBitrate, "-maxrate", tc.videoMaxrate, "-bufsize", tc.videoBufsize,
				)

			case "h264_qsv":
				if vf := buildVideoFilter(enc.codec, highBitDepth, outW, outH, scaled); vf != "" {
					a = append(a, "-vf", vf)
				}
				a = append(a,
					"-c:v", "h264_qsv", "-preset", "veryfast",
					"-g", gopStr,
					"-b:v", tc.videoBitrate, "-maxrate", tc.videoMaxrate, "-bufsize", tc.videoBufsize,
				)

			case "h264_videotoolbox":
				if vf := buildVideoFilter(enc.codec, highBitDepth, outW, outH, scaled); vf != "" {
					a = append(a, "-vf", vf)
				}
				a = append(a,
					"-c:v", "h264_videotoolbox", "-realtime", "1",
					"-g", gopStr,
					"-b:v", tc.videoBitrate, "-maxrate", tc.videoMaxrate, "-bufsize", tc.videoBufsize,
				)

			case "h264_v4l2m2m":
				if vf := buildVideoFilter(enc.codec, highBitDepth, outW, outH, scaled); vf != "" {
					a = append(a, "-vf", vf)
				}
				a = append(a,
					"-c:v", "h264_v4l2m2m",
					"-g", gopStr,
					"-b:v", tc.videoBitrate, "-maxrate", tc.videoMaxrate, "-bufsize", tc.videoBufsize,
				)

			default: // libx264 — quality-constrained CRF with bitrate ceiling
				a = append(a,
					"-vf", buildVideoFilter("libx264", true, outW, outH, scaled), // ensure 8-bit even for 10-bit inputs
					"-c:v", "libx264", "-preset", tc.x264Preset, "-crf", strconv.Itoa(m.cfg.X264CRF),
					"-profile:v", "high",
					// sc_threshold=0 disables scene-cut detection so -g is strictly
					// honoured; -keyint_min enforces IDR at every GOP boundary.
					"-sc_threshold", "0",
					"-g", gopStr, "-keyint_min", gopStr,
					"-b:v", tc.videoBitrate, "-maxrate", tc.videoMaxrate, "-bufsize", tc.videoBufsize,
				)
			}
		}

		// ── Audio encoding (segMuxed and segAudioOnly) ────────────────────────
		// aresample=async=1:first_pts=0 realigns audio to presentation timestamps
		// after a seek; apad pads short final segments to prevent under-runs.
		switch kind {
		case segMuxed:
			a = append(a,
				"-c:a", "aac", "-ac", strconv.Itoa(m.cfg.AudioChannels), "-b:a", m.cfg.AudioBitrate,
				"-af", "aresample=async=1:first_pts=0,apad",
				"-sn", // drop subtitle streams from the output
			)
		case segAudioOnly:
			a = append(a,
				"-c:a", "aac", "-ac", strconv.Itoa(m.cfg.AudioChannels), "-b:a", m.cfg.AudioBitrate,
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

		ctx, cancel := context.WithTimeout(ctx, m.cfg.SegmentTimeout)
		defer cancel()
		return exec.CommandContext(ctx, "ffmpeg", a...).Run()
	}

	// Attempt hardware encode; fall back to libx264 on any error.
	// If the caller's context is already done when the HW attempt fails,
	// propagate the cancellation directly — do not start a software re-encode
	// for a segment that is no longer needed.
	sw := hwEncoder{codec: "libx264"}
	var err error
	if m.enc.isHW && tc.hwEnabled && kind != segAudioOnly {
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
		_ = os.Remove(tmp)
		return "", err
	}
	return segFile, nil
}

// ── CloseHLS ──────────────────────────────────────────────────────────────────

// CloseHLS stops the background reaper and removes the manager's entire base
// working directory (created fresh per-manager by newHLSBaseDir in newHLS, so
// removing it cannot affect any other manager or process). Safe to call
// multiple times.
func (m *hlsManager) CloseHLS() {
	// Signal the reaper to stop; guard against double-close with a non-blocking
	// drain: if stopCh is already closed the receive arm fires immediately.
	select {
	case <-m.stopCh:
	default:
		close(m.stopCh)
	}
	m.mu.Lock()
	m.sessions = map[string]*hlsSession{}
	m.mu.Unlock()
	// RemoveAll outside the lock: disk I/O must not stall concurrent requests.
	_ = os.RemoveAll(m.base)
}

// reaper is the single background goroutine that evicts idle HLS sessions.
// It runs until CloseHLS closes stopCh, so it never leaks.
func (m *hlsManager) reaper() {
	ticker := time.NewTicker(m.cfg.ReaperInterval)
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

// evictIdle removes sessions whose lastAccess timestamp is older than
// HLSConfig.SessionTTL AND that have no work currently in flight. Eviction
// predicate:
//
//	inFlight == 0  &&  lastAccess != 0  &&  lastAccess < now-SessionTTL
//
// A session with inFlight > 0 (an HLSFile call — segment transcode, subtitle
// extraction, or playlist write — is still running) is never evicted no
// matter how stale lastAccess looks: deleting it mid-write would RemoveAll
// the directory ffmpeg is actively writing seg<n>.ts.tmp.ts into, breaking
// the in-flight request and orphaning the session id ("unknown session") for
// every subsequent request. Deleting from a map during range is safe and
// defined in Go.
func (m *hlsManager) evictIdle() {
	cutoff := time.Now().Add(-m.cfg.SessionTTL)
	var victims []string
	m.mu.Lock()
	for id, s := range m.sessions {
		if s.inFlight.Load() > 0 {
			// Work is actively running for this session; skip regardless of
			// how old lastAccess is. HLSFile refreshes lastAccess again on
			// exit, so once the work finishes the session gets a fresh idle
			// window before the next reaper pass can evict it.
			continue
		}
		ts := s.lastAccess.Load()
		if ts == 0 {
			// Session created but not yet accessed (e.g. in the window between
			// map insertion and the first lastAccess.Store); skip to be safe.
			continue
		}
		if time.Unix(0, ts).Before(cutoff) {
			delete(m.sessions, id)
			victims = append(victims, s.dir)
		}
	}
	m.mu.Unlock()
	// RemoveAll outside the lock: disk I/O must not stall concurrent requests.
	for _, dir := range victims {
		_ = os.RemoveAll(dir)
	}
}

// sweepProbeCache removes expired entries from the negative probe cache and,
// when the size still exceeds probeCacheMaxSize after the TTL sweep, evicts
// the entry with the smallest remaining TTL.  Must be called with m.mu held.
func (m *hlsManager) sweepProbeCache() {
	now := time.Now()
	for k, e := range m.probeCache {
		if now.After(e.expiresAt) {
			delete(m.probeCache, k)
		}
	}
	// Hard size cap: evict soonest-expiring entry until under limit.
	for len(m.probeCache) > probeCacheMaxSize {
		var evict string
		var evictExp time.Time
		for k, e := range m.probeCache {
			if evict == "" || e.expiresAt.Before(evictExp) {
				evict = k
				evictExp = e.expiresAt
			}
		}
		delete(m.probeCache, evict)
	}
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
