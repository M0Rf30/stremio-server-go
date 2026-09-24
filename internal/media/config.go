package media

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// HLSConfig holds every runtime-tunable knob for the HLS transcode manager
// (issue #20). Every field defaults to the value that was previously a
// compile-time constant, so DefaultHLSConfig() reproduces today's behaviour
// exactly. Values are resolved once by internal/app (via Lookup — env vars
// and, in library mode, the envJSON map) and handed to New()/newHLS() as a
// plain struct; internal/media never calls os.Getenv itself.
type HLSConfig struct {
	// --- session lifecycle ---
	SessionTTL     time.Duration // idle-eviction window (STREMIO_HLS_SESSION_TTL)
	ReaperInterval time.Duration // reaper scan period (STREMIO_HLS_REAPER_INTERVAL)
	NegProbeTTL    time.Duration // failed-probe negative cache TTL (STREMIO_HLS_NEG_PROBE_TTL)
	PosProbeTTL    time.Duration // successful-probe cache TTL (STREMIO_HLS_POS_PROBE_TTL)
	MaxSessions    int           // hard cap on concurrent sessions (STREMIO_HLS_MAX_SESSIONS)
	WorkDir        string        // "" = os.MkdirTemp default; else stremio-hls-* under it (STREMIO_HLS_WORK_DIR)

	// --- output quality / bandwidth ---
	VideoBitrate string // -b:v, e.g. "8M" (STREMIO_TRANSCODE_VIDEO_BITRATE)
	VideoMaxrate string // -maxrate (STREMIO_TRANSCODE_MAXRATE)
	VideoBufsize string // -bufsize (STREMIO_TRANSCODE_BUFSIZE)
	MaxWidth     int    // 0 = no downscale (STREMIO_TRANSCODE_MAX_WIDTH)
	MaxHeight    int    // 0 = no downscale (STREMIO_TRANSCODE_MAX_HEIGHT)

	// --- encoder tuning ---
	VAAPIQP     int    // -qp for h264_vaapi (STREMIO_TRANSCODE_VAAPI_QP)
	NVENCPreset string // -preset for h264_nvenc (STREMIO_TRANSCODE_NVENC_PRESET)
	X264Preset  string // -preset for libx264 (STREMIO_TRANSCODE_X264_PRESET)
	X264CRF     int    // -crf for libx264 (STREMIO_TRANSCODE_X264_CRF)

	// --- audio ---
	AudioChannels int    // -ac (STREMIO_TRANSCODE_AUDIO_CHANNELS)
	AudioBitrate  string // -b:a (STREMIO_TRANSCODE_AUDIO_BITRATE)

	// --- concurrency / hardware ---
	SegmentConcurrency int    // concurrent ffmpeg segment jobs; 0 resolves to runtime.NumCPU() (STREMIO_TRANSCODE_CONCURRENCY)
	VAAPIDevice        string // renderD* device path (STREMIO_HLS_VAAPI_DEVICE)

	// --- timeouts ---
	SegmentTimeout  time.Duration // per-segment transcode (STREMIO_HLS_SEGMENT_TIMEOUT)
	SubtitleTimeout time.Duration // subtitle extraction (STREMIO_HLS_SUBTITLE_TIMEOUT)
	ProbeTimeout    time.Duration // ffprobe combined probe (STREMIO_HLS_PROBE_TIMEOUT)
}

// Historical compile-time defaults, kept as named constants so
// DefaultHLSConfig and the default-compatibility check in
// deriveBandwidthCodecs share a single source of truth.
const (
	defaultSessionTTL     = 60 * time.Second
	defaultReaperInterval = 30 * time.Second
	defaultNegProbeTTL    = 5 * time.Minute
	defaultPosProbeTTL    = 10 * time.Minute
	defaultMaxSessions    = 64
	defaultVideoBitrate   = "8M"
	defaultVideoMaxrate   = "8M"
	defaultVideoBufsize   = "16M"
	defaultVAAPIQP        = 23
	defaultNVENCPreset    = "p4"
	defaultX264Preset     = "veryfast"
	defaultX264CRF        = 23
	defaultAudioChannels  = 2
	defaultAudioBitrate   = "192k"
	defaultVAAPIDevice    = "/dev/dri/renderD128"
	defaultSegmentTimeout = 120 * time.Second
	defaultSubtitleTTL    = 120 * time.Second
	defaultProbeTimeout   = 30 * time.Second

	// legacyMaxrateBps is defaultVideoMaxrate ("8M") parsed to bits/second.
	// Compared against verbatim in deriveBandwidthCodecs to detect the fully
	// unconfigured case.
	legacyMaxrateBps int64 = 8_000_000
	// legacyBandwidth/legacyCodecsVideo are the exact master-playlist values
	// this server has always advertised (see hls.go StartHLS history). They
	// must keep being emitted byte-for-byte whenever nothing about the
	// transcode configuration has changed, regardless of the actual source
	// resolution — deriving purely mathematically would change the string
	// for the many real sources that aren't exactly 1920x1080/8Mbps.
	legacyBandwidth   int64  = 4_000_000
	legacyCodecsVideo string = "avc1.640029" // H.264 Level 4.1
)

// DefaultHLSConfig returns the config that reproduces every previously
// hardcoded default byte-for-byte. SegmentConcurrency is left at 0 (meaning
// "resolve to runtime.NumCPU() at construction") since that resolution needs
// the runtime package, which callers already have.
func DefaultHLSConfig() HLSConfig {
	return HLSConfig{
		SessionTTL:         defaultSessionTTL,
		ReaperInterval:     defaultReaperInterval,
		NegProbeTTL:        defaultNegProbeTTL,
		PosProbeTTL:        defaultPosProbeTTL,
		MaxSessions:        defaultMaxSessions,
		WorkDir:            "",
		VideoBitrate:       defaultVideoBitrate,
		VideoMaxrate:       defaultVideoMaxrate,
		VideoBufsize:       defaultVideoBufsize,
		MaxWidth:           0,
		MaxHeight:          0,
		VAAPIQP:            defaultVAAPIQP,
		NVENCPreset:        defaultNVENCPreset,
		X264Preset:         defaultX264Preset,
		X264CRF:            defaultX264CRF,
		AudioChannels:      defaultAudioChannels,
		AudioBitrate:       defaultAudioBitrate,
		SegmentConcurrency: 0,
		VAAPIDevice:        defaultVAAPIDevice,
		SegmentTimeout:     defaultSegmentTimeout,
		SubtitleTimeout:    defaultSubtitleTTL,
		ProbeTimeout:       defaultProbeTimeout,
	}
}

// DefaultReaperInterval derives the reaper's default scan interval from the
// session idle-eviction TTL: min(30s, ttl/2). Exported so internal/app can
// compute the default before applying STREMIO_HLS_REAPER_INTERVAL as an
// explicit override (issue #20: "Should probably scale with the TTL above"),
// giving the derivation a single, unit-tested home instead of duplicating it
// in internal/app.
func DefaultReaperInterval(ttl time.Duration) time.Duration {
	half := ttl / 2
	if half <= 0 || half > defaultReaperInterval {
		return defaultReaperInterval
	}
	return half
}

// normalize fills in any zero-value fields left unset by a caller-built
// HLSConfig (e.g. a test constructing a literal) with DefaultHLSConfig's
// values, and resolves SegmentConcurrency=0 to numCPU. Called once by
// newHLS(); every hlsManager method reads a fully-resolved m.cfg afterward.
func (c HLSConfig) normalize(numCPU int) HLSConfig {
	d := DefaultHLSConfig()
	if c.SessionTTL <= 0 {
		c.SessionTTL = d.SessionTTL
	}
	if c.ReaperInterval <= 0 {
		c.ReaperInterval = DefaultReaperInterval(c.SessionTTL)
	}
	if c.NegProbeTTL <= 0 {
		c.NegProbeTTL = d.NegProbeTTL
	}
	if c.PosProbeTTL <= 0 {
		c.PosProbeTTL = d.PosProbeTTL
	}
	if c.MaxSessions <= 0 {
		c.MaxSessions = d.MaxSessions
	}
	if c.VideoBitrate == "" {
		c.VideoBitrate = d.VideoBitrate
	}
	if c.VideoMaxrate == "" {
		c.VideoMaxrate = d.VideoMaxrate
	}
	if c.VideoBufsize == "" {
		c.VideoBufsize = d.VideoBufsize
	}
	if c.VAAPIQP <= 0 {
		c.VAAPIQP = d.VAAPIQP
	}
	if c.NVENCPreset == "" {
		c.NVENCPreset = d.NVENCPreset
	}
	if c.X264Preset == "" {
		c.X264Preset = d.X264Preset
	}
	if c.X264CRF <= 0 {
		c.X264CRF = d.X264CRF
	}
	if c.AudioChannels <= 0 {
		c.AudioChannels = d.AudioChannels
	}
	if c.AudioBitrate == "" {
		c.AudioBitrate = d.AudioBitrate
	}
	if c.SegmentConcurrency <= 0 {
		if numCPU < 1 {
			numCPU = 1
		}
		c.SegmentConcurrency = numCPU
	}
	if c.VAAPIDevice == "" {
		c.VAAPIDevice = d.VAAPIDevice
	}
	if c.SegmentTimeout <= 0 {
		c.SegmentTimeout = d.SegmentTimeout
	}
	if c.SubtitleTimeout <= 0 {
		c.SubtitleTimeout = d.SubtitleTimeout
	}
	if c.ProbeTimeout <= 0 {
		c.ProbeTimeout = d.ProbeTimeout
	}
	// MaxWidth/MaxHeight/WorkDir: 0/"" is a meaningful value (no downscale /
	// OS-default temp dir), not "unset" — left as provided.
	return c
}

// SettingsSource is the minimal surface hlsManager needs from
// types.SettingsStore to apply live /settings overrides at session-creation
// time (issue #20 "Related": transcodeMaxBitRate, transcodeMaxWidth,
// transcodeConcurrency, transcodeHardwareAccel, transcodeProfile).
// types.SettingsStore satisfies this structurally, so callers pass it
// directly without internal/media importing anything beyond what it already
// does.
type SettingsSource interface {
	Get(key string) interface{}
}

// settingsInt reads key as an int, accepting the float64/int/int64 shapes
// Get may return depending on whether the value came from json.Unmarshal (a
// loaded server-settings.json) or a fresh in-memory default.
func settingsInt(ss SettingsSource, key string) (int, bool) {
	if ss == nil {
		return 0, false
	}
	switch v := ss.Get(key).(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	}
	return 0, false
}

func settingsBool(ss SettingsSource, key string) (bool, bool) {
	if ss == nil {
		return false, false
	}
	if v, ok := ss.Get(key).(bool); ok {
		return v, true
	}
	return false, false
}

func settingsString(ss SettingsSource, key string) (string, bool) {
	if ss == nil {
		return "", false
	}
	if v, ok := ss.Get(key).(string); ok && v != "" {
		return v, true
	}
	return "", false
}

// sessionConfig is the subset of HLSConfig that can vary per session because
// a live /settings value overrides it. Snapshotted once in StartHLS (session
// creation) so an already-open session keeps behaving consistently even if
// /settings changes again while it's playing, matching how transcodeSegment
// already treats every other per-session field as immutable after creation.
type sessionConfig struct {
	videoBitrate string
	videoMaxrate string
	videoBufsize string
	maxrateBps   int64 // parsed bits/second, used for BANDWIDTH derivation
	maxWidth     int
	maxHeight    int
	hwEnabled    bool
	x264Preset   string
	nvencPreset  string
}

// x264PresetOrder lists every libx264 preset name from fastest/lowest-quality
// to slowest/highest-quality. transcodeProfile is validated in api.go only as
// "a string or null" (no fixed enum), so these are the values this server
// treats as recognized profiles — matching libx264's own preset names keeps
// the setting portable/self-documenting instead of inventing new vocabulary.
var x264PresetOrder = []string{
	"ultrafast", "superfast", "veryfast", "faster", "fast", "medium", "slow", "slower", "veryslow",
}

// nvencPresetForX264 maps a libx264 preset name to the closest NVENC preset,
// so a single transcodeProfile value drives both software and NVENC hardware
// encodes consistently. "veryfast" (the default profile) maps to "p4" — the
// existing NVENC default — so leaving transcodeProfile unset changes nothing.
func nvencPresetForX264(x264Preset string) string {
	switch x264Preset {
	case "ultrafast", "superfast":
		return "p1"
	case "veryfast":
		return "p4"
	case "faster":
		return "p3"
	case "fast":
		return "p4"
	case "medium":
		return "p5"
	case "slow":
		return "p6"
	case "slower", "veryslow":
		return "p7"
	default:
		return defaultNVENCPreset
	}
}

// isKnownX264Preset reports whether profile is one of x264PresetOrder.
func isKnownX264Preset(profile string) bool {
	for _, p := range x264PresetOrder {
		if p == profile {
			return true
		}
	}
	return false
}

// effectiveSessionConfig computes the per-session transcode configuration at
// session-creation time (StartHLS), applying live /settings overrides on top
// of m.cfg's env-resolved defaults. Precedence, per key:
//
//   - transcodeMaxBitRate (settings default 0 = "unset"): overrides
//     videoBitrate/videoMaxrate/videoBufsize (bufsize = 2x) when > 0.
//   - transcodeMaxWidth (settings default 1920 — already the schema's
//     baked-in fresh-install value, NOT a sentinel): overrides MaxWidth only
//     when > 0 AND != 1920, so a server whose /settings was never touched
//     keeps today's uncapped-resolution output byte-for-byte. An operator who
//     wants exactly 1920 as an intentional cap should set
//     STREMIO_TRANSCODE_MAX_WIDTH=1920 instead — indistinguishable from
//     "untouched" is the unavoidable cost of the byte-identical-default
//     requirement, since the settings store cannot tell "user re-saved 1920"
//     from "user never saved anything".
//   - transcodeHardwareAccel (settings default true): false forces software
//     encoding for this session; true (default) preserves the existing
//     auto-detected m.enc behaviour unchanged.
//   - transcodeProfile (settings default nil): a recognized libx264 preset
//     name overrides both X264Preset and (mapped) NVENCPreset.
func (m *hlsManager) effectiveSessionConfig() sessionConfig {
	cfg := m.cfg
	sc := sessionConfig{
		videoBitrate: cfg.VideoBitrate,
		videoMaxrate: cfg.VideoMaxrate,
		videoBufsize: cfg.VideoBufsize,
		maxWidth:     cfg.MaxWidth,
		maxHeight:    cfg.MaxHeight,
		hwEnabled:    true,
		x264Preset:   cfg.X264Preset,
		nvencPreset:  cfg.NVENCPreset,
	}
	sc.maxrateBps, _ = parseFFmpegBitrate(cfg.VideoMaxrate)

	if bps, ok := settingsInt(m.settings, "transcodeMaxBitRate"); ok && bps > 0 {
		v := int64(bps)
		sc.maxrateBps = v
		bpsStr := strconv.FormatInt(v, 10)
		sc.videoBitrate = bpsStr
		sc.videoMaxrate = bpsStr
		sc.videoBufsize = strconv.FormatInt(v*2, 10)
	}

	if w, ok := settingsInt(m.settings, "transcodeMaxWidth"); ok && w > 0 && w != 1920 {
		sc.maxWidth = w
	}

	if hw, ok := settingsBool(m.settings, "transcodeHardwareAccel"); ok {
		sc.hwEnabled = hw
	}

	if profile, ok := settingsString(m.settings, "transcodeProfile"); ok {
		profile = strings.ToLower(strings.TrimSpace(profile))
		if isKnownX264Preset(profile) {
			sc.x264Preset = profile
			sc.nvencPreset = nvencPresetForX264(profile)
		}
	}

	return sc
}

// isDefault reports whether sc matches the fully-unconfigured (no env
// override, no /settings override) state used by deriveBandwidthCodecs to
// decide whether to reproduce the legacy playlist strings verbatim.
func (sc sessionConfig) isDefaultMaxrate() bool {
	return sc.maxrateBps == legacyMaxrateBps
}

// ── bitrate / resolution helpers ────────────────────────────────────────────

// ffmpegBitrateValueRe matches a bare ffmpeg bitrate value: digits with an
// optional k/K/m/M/g/G (decimal SI) suffix.
var ffmpegBitrateValueRe = regexp.MustCompile(`^([0-9]+)([kKmMgG]?)$`)

// parseFFmpegBitrate converts an ffmpeg -b:v/-maxrate-style string ("8M",
// "800k", "8000000") to bits/second. ok is false for anything that doesn't
// match ffmpeg's own bitrate syntax (fractional values are accepted by
// ffmpeg but never produced by this codebase's config, so they're treated as
// unparseable here rather than adding float handling nothing exercises).
func parseFFmpegBitrate(s string) (int64, bool) {
	m := ffmpegBitrateValueRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	switch strings.ToLower(m[2]) {
	case "k":
		n *= 1_000
	case "m":
		n *= 1_000_000
	case "g":
		n *= 1_000_000_000
	}
	return n, true
}

// computeScaledDims applies the MaxWidth/MaxHeight downscale caps to a
// source resolution: preserves aspect ratio (uniform scale factor), never
// upscales, and forces both output dimensions even (required by the
// yuv420p/nv12 pixel formats every encoder path in this package uses).
// scaled is false whenever no cap applies or the source already fits within
// both caps, in which case (w, h) == (srcW, srcH).
func computeScaledDims(srcW, srcH, maxW, maxH int) (w, h int, scaled bool) {
	if srcW <= 0 || srcH <= 0 || (maxW <= 0 && maxH <= 0) {
		return srcW, srcH, false
	}
	scale := 1.0
	if maxW > 0 {
		if s := float64(maxW) / float64(srcW); s < scale {
			scale = s
		}
	}
	if maxH > 0 {
		if s := float64(maxH) / float64(srcH); s < scale {
			scale = s
		}
	}
	if scale >= 1.0 {
		return srcW, srcH, false // cap(s) already satisfied; never upscale
	}
	w = int(math.Floor(float64(srcW) * scale))
	h = int(math.Floor(float64(srcH) * scale))
	w -= w % 2
	h -= h % 2
	if w < 2 {
		w = 2
	}
	if h < 2 {
		h = 2
	}
	return w, h, true
}

// h264Level maps an output resolution to the AVC profile-level CODECS string
// used in the HLS master playlist, per issue #20's table. Unknown (<=0)
// dimensions keep the historical Level 4.1 string rather than guessing.
func h264Level(width, height int) string {
	if width <= 0 || height <= 0 {
		return legacyCodecsVideo
	}
	area := width * height
	switch {
	case area <= 1280*720:
		return "avc1.64001f" // Level 3.1
	case area <= 1920*1080:
		return "avc1.640029" // Level 4.1
	case area <= 2560*1440:
		return "avc1.640032" // Level 5.0
	default:
		return "avc1.640033" // Level 5.1
	}
}

// deriveBandwidthCodecs computes the master-playlist BANDWIDTH and video
// CODECS entry from the effective bitrate cap (maxrateBps, bits/second) and
// output resolution (post-downscale). When the configuration is exactly the
// historical default — maxrateBps still the original 8M constant and no
// downscale actually applied to this stream — it returns the legacy fixed
// values verbatim (see legacyBandwidth/legacyCodecsVideo) instead of the
// generally-derived ones, so unconfigured output never changes regardless of
// actual source resolution. Otherwise BANDWIDTH = maxrateBps/2 (preserving
// the original 8M-cap -> 4,000,000-BANDWIDTH ratio as the general rule, not
// just the default case) and CODECS is the resolution-derived level.
func deriveBandwidthCodecs(sc sessionConfig, outW, outH int, downscaled bool) (bandwidth int64, codecsVideo string) {
	if sc.isDefaultMaxrate() && !downscaled {
		return legacyBandwidth, legacyCodecsVideo
	}
	bandwidth = sc.maxrateBps / 2
	if bandwidth <= 0 {
		bandwidth = legacyBandwidth
	}
	return bandwidth, h264Level(outW, outH)
}

// buildVideoFilter assembles the -vf filter chain for one encoder backend,
// combining the existing high-bit-depth pixel-format conversion with an
// optional downscale filter using the correct filter name per backend:
//   - h264_vaapi already builds a hw frame (format=nv12,hwupload in the
//     caller); scale_vaapi operates on that hw frame and must come after it.
//   - every other backend (libx264, h264_nvenc, h264_qsv,
//     h264_videotoolbox, h264_v4l2m2m) in this codebase decodes and filters
//     entirely in system memory (no -hwaccel/hwupload is ever added — see
//     transcodeSegment's high-bit-depth-safety note), so a plain CPU "scale"
//     filter matches their existing chain style; scale_cuda/scale_npp would
//     need a CUDA/NPP hw-frames context this codebase never establishes.
func buildVideoFilter(codec string, needsFormatConv bool, scaledW, scaledH int, scaled bool) string {
	var filters []string
	switch codec {
	case "h264_vaapi":
		filters = append(filters, "format=nv12", "hwupload")
		if scaled {
			filters = append(filters, fmt.Sprintf("scale_vaapi=w=%d:h=%d", scaledW, scaledH))
		}
	default:
		if needsFormatConv {
			filters = append(filters, "format=yuv420p")
		}
		if scaled {
			filters = append(filters, fmt.Sprintf("scale=%d:%d", scaledW, scaledH))
		}
	}
	return strings.Join(filters, ",")
}
