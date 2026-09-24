package media

// Tests for internal/media/config.go (issue #20): HLSConfig defaults/
// normalization, reaper-interval derivation, bitrate/resolution helpers,
// live /settings precedence, downscale filter chains, and the master
// playlist golden/derived BANDWIDTH+CODECS behaviour.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// stubSettings is a minimal SettingsSource backed by a plain map, for tests
// that need to exercise live /settings precedence without a real
// types.SettingsStore.
type stubSettings map[string]interface{}

func (s stubSettings) Get(key string) interface{} {
	if s == nil {
		return nil
	}
	return s[key]
}

// ── DefaultHLSConfig / normalize ────────────────────────────────────────────

func TestDefaultHLSConfigMatchesHistoricalConstants(t *testing.T) {
	d := DefaultHLSConfig()
	cases := map[string]bool{
		"SessionTTL":     d.SessionTTL == 60*time.Second,
		"ReaperInterval": d.ReaperInterval == 30*time.Second,
		"NegProbeTTL":    d.NegProbeTTL == 5*time.Minute,
		"PosProbeTTL":    d.PosProbeTTL == 10*time.Minute,
		"MaxSessions":    d.MaxSessions == 64,
		"VideoBitrate":   d.VideoBitrate == "8M",
		"VideoMaxrate":   d.VideoMaxrate == "8M",
		"VideoBufsize":   d.VideoBufsize == "16M",
		"MaxWidth":       d.MaxWidth == 0,
		"MaxHeight":      d.MaxHeight == 0,
		"VAAPIQP":        d.VAAPIQP == 23,
		"NVENCPreset":    d.NVENCPreset == "p4",
		"X264Preset":     d.X264Preset == "veryfast",
		"X264CRF":        d.X264CRF == 23,
		"AudioChannels":  d.AudioChannels == 2,
		"AudioBitrate":   d.AudioBitrate == "192k",
		"VAAPIDevice":    d.VAAPIDevice == "/dev/dri/renderD128",
		"SegmentTimeout": d.SegmentTimeout == 120*time.Second,
		"SubtitleTTL":    d.SubtitleTimeout == 120*time.Second,
		"ProbeTimeout":   d.ProbeTimeout == 30*time.Second,
	}
	for name, ok := range cases {
		if !ok {
			t.Errorf("DefaultHLSConfig().%s does not match the historical constant", name)
		}
	}
}

func TestDefaultReaperInterval(t *testing.T) {
	cases := []struct {
		ttl  time.Duration
		want time.Duration
	}{
		{60 * time.Second, 30 * time.Second},      // issue #20 example: min(30s, 60/2) = 30s
		{10 * time.Second, 5 * time.Second},       // scales down with a short TTL
		{300 * time.Second, 30 * time.Second},     // capped at 30s for a long TTL
		{0, 30 * time.Second},                     // degenerate: falls back to 30s
		{1 * time.Second, 500 * time.Millisecond}, // half (500ms) is under the 30s cap
	}
	for _, c := range cases {
		if got := DefaultReaperInterval(c.ttl); got != c.want {
			t.Errorf("DefaultReaperInterval(%s) = %s, want %s", c.ttl, got, c.want)
		}
	}
}

func TestHLSConfigNormalizeFillsZeroValues(t *testing.T) {
	var zero HLSConfig
	got := zero.normalize(4)
	want := DefaultHLSConfig()
	want.SegmentConcurrency = 4 // resolved from the numCPU param
	want.ReaperInterval = DefaultReaperInterval(want.SessionTTL)
	if got != want {
		t.Errorf("normalize(zero) = %+v, want %+v", got, want)
	}
}

func TestHLSConfigNormalizePreservesExplicitZeroMaxWidthHeight(t *testing.T) {
	cfg := DefaultHLSConfig()
	cfg.MaxWidth = 0
	cfg.MaxHeight = 0
	got := cfg.normalize(2)
	if got.MaxWidth != 0 || got.MaxHeight != 0 {
		t.Errorf("normalize must preserve MaxWidth/MaxHeight=0 (no downscale), got %d/%d", got.MaxWidth, got.MaxHeight)
	}
}

func TestHLSConfigNormalizeSegmentConcurrencyFloor(t *testing.T) {
	var zero HLSConfig
	got := zero.normalize(0) // pathological runtime.NumCPU() == 0
	if got.SegmentConcurrency != 1 {
		t.Errorf("SegmentConcurrency = %d, want floor of 1", got.SegmentConcurrency)
	}
}

// ── parseFFmpegBitrate ───────────────────────────────────────────────────────

func TestParseFFmpegBitrate(t *testing.T) {
	cases := []struct {
		in     string
		want   int64
		wantOK bool
	}{
		{"8M", 8_000_000, true},
		{"800k", 800_000, true},
		{"800K", 800_000, true},
		{"8000000", 8_000_000, true},
		{"1G", 1_000_000_000, true},
		{"", 0, false},
		{"abc", 0, false},
		{"8Mi", 0, false}, // binary-multiple suffix not supported by this codebase
		{"-8M", 0, false},
	}
	for _, c := range cases {
		got, ok := parseFFmpegBitrate(c.in)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("parseFFmpegBitrate(%q) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

// ── computeScaledDims ────────────────────────────────────────────────────────

func TestComputeScaledDims(t *testing.T) {
	cases := []struct {
		name                   string
		srcW, srcH, maxW, maxH int
		wantW, wantH           int
		wantScaled             bool
	}{
		{"no caps configured", 3840, 2160, 0, 0, 3840, 2160, false},
		{"source already within caps", 1280, 720, 1920, 1080, 1280, 720, false},
		{"never upscale", 640, 360, 1920, 1080, 640, 360, false},
		{"4k capped to 1080p width, aspect preserved", 3840, 2160, 1920, 0, 1920, 1080, true},
		{"height cap drives the scale", 1920, 1080, 0, 720, 1280, 720, true},
		{"both caps, width is the binding constraint", 3840, 2160, 1280, 1000, 1280, 720, true},
		{"odd source dims still produce even output", 1921, 1081, 960, 0, 960, 540, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, h, scaled := computeScaledDims(c.srcW, c.srcH, c.maxW, c.maxH)
			if w != c.wantW || h != c.wantH || scaled != c.wantScaled {
				t.Errorf("computeScaledDims(%d,%d,%d,%d) = (%d,%d,%v), want (%d,%d,%v)",
					c.srcW, c.srcH, c.maxW, c.maxH, w, h, scaled, c.wantW, c.wantH, c.wantScaled)
			}
			if w%2 != 0 || h%2 != 0 {
				t.Errorf("computeScaledDims output must be even, got %dx%d", w, h)
			}
			if scaled && (w > c.srcW || h > c.srcH) {
				t.Errorf("computeScaledDims must never upscale: src %dx%d, got %dx%d", c.srcW, c.srcH, w, h)
			}
		})
	}
}

// ── h264Level / deriveBandwidthCodecs ───────────────────────────────────────

func TestH264Level(t *testing.T) {
	cases := []struct {
		w, h int
		want string
	}{
		{1280, 720, "avc1.64001f"},
		{1920, 1080, "avc1.640029"},
		{2560, 1440, "avc1.640032"},
		{3840, 2160, "avc1.640033"},
		{640, 360, "avc1.64001f"},
		{0, 0, legacyCodecsVideo},
		{-1, 100, legacyCodecsVideo},
	}
	for _, c := range cases {
		if got := h264Level(c.w, c.h); got != c.want {
			t.Errorf("h264Level(%d,%d) = %q, want %q", c.w, c.h, got, c.want)
		}
	}
}

// TestDeriveBandwidthCodecsDefaultCompat is the regression test for issue
// #20's compatibility requirement: an unconfigured server must keep
// advertising BANDWIDTH=4000000 and CODECS="avc1.640029,..." verbatim,
// regardless of the actual (unknown/arbitrary) source resolution — deriving
// purely mathematically from a non-1080p source would otherwise change the
// string for the majority of real content this server has always served
// identically.
func TestDeriveBandwidthCodecsDefaultCompat(t *testing.T) {
	sc := sessionConfig{maxrateBps: legacyMaxrateBps}
	for _, res := range [][2]int{{0, 0}, {640, 360}, {1280, 720}, {3840, 2160}} {
		bw, codecs := deriveBandwidthCodecs(sc, res[0], res[1], false)
		if bw != legacyBandwidth || codecs != legacyCodecsVideo {
			t.Errorf("deriveBandwidthCodecs(default, %dx%d, downscaled=false) = (%d,%q), want (%d,%q)",
				res[0], res[1], bw, codecs, legacyBandwidth, legacyCodecsVideo)
		}
	}
}

func TestDeriveBandwidthCodecsDerivedForNonDefaultConfig(t *testing.T) {
	cases := []struct {
		name       string
		sc         sessionConfig
		outW, outH int
		downscaled bool
		wantBW     int64
		wantCodecs string
	}{
		{
			name: "custom higher bitrate cap, no downscale",
			sc:   sessionConfig{maxrateBps: 16_000_000},
			outW: 1920, outH: 1080,
			downscaled: false,
			wantBW:     8_000_000, // maxrateBps/2, same ratio as the legacy default
			wantCodecs: "avc1.640029",
		},
		{
			name: "default bitrate but actively downscaled to 720p",
			sc:   sessionConfig{maxrateBps: legacyMaxrateBps},
			outW: 1280, outH: 720,
			downscaled: true,
			wantBW:     4_000_000, // maxrateBps/2 == legacy value here, but via the derived path
			wantCodecs: "avc1.64001f",
		},
		{
			name: "lower bitrate cap, downscaled to 1440p-class output",
			sc:   sessionConfig{maxrateBps: 5_000_000},
			outW: 2560, outH: 1440,
			downscaled: true,
			wantBW:     2_500_000,
			wantCodecs: "avc1.640032",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bw, codecs := deriveBandwidthCodecs(c.sc, c.outW, c.outH, c.downscaled)
			if bw != c.wantBW || codecs != c.wantCodecs {
				t.Errorf("deriveBandwidthCodecs(...) = (%d,%q), want (%d,%q)", bw, codecs, c.wantBW, c.wantCodecs)
			}
		})
	}
}

// ── buildVideoFilter (downscale filter chains per backend) ─────────────────

func TestBuildVideoFilter(t *testing.T) {
	cases := []struct {
		name            string
		codec           string
		needsFormatConv bool
		scaled          bool
		want            string
	}{
		{"vaapi always uploads, never scaled", "h264_vaapi", true, false, "format=nv12,hwupload"},
		{"vaapi scaled uses scale_vaapi after hwupload", "h264_vaapi", true, true, "format=nv12,hwupload,scale_vaapi=w=1280:h=720"},
		{"libx264 default: format only, no scale", "libx264", true, false, "format=yuv420p"},
		{"libx264 scaled: format then plain scale", "libx264", true, true, "format=yuv420p,scale=1280:720"},
		{"nvenc 8-bit unscaled: no filter at all", "h264_nvenc", false, false, ""},
		{"nvenc high-bit-depth unscaled", "h264_nvenc", true, false, "format=yuv420p"},
		{"nvenc scaled 8-bit uses plain scale (no scale_cuda/scale_npp chain)", "h264_nvenc", false, true, "scale=1280:720"},
		{"qsv scaled", "h264_qsv", false, true, "scale=1280:720"},
		{"videotoolbox scaled", "h264_videotoolbox", false, true, "scale=1280:720"},
		{"v4l2m2m scaled", "h264_v4l2m2m", false, true, "scale=1280:720"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildVideoFilter(c.codec, c.needsFormatConv, 1280, 720, c.scaled)
			if got != c.want {
				t.Errorf("buildVideoFilter(%q, conv=%v, scaled=%v) = %q, want %q", c.codec, c.needsFormatConv, c.scaled, got, c.want)
			}
		})
	}
}

// ── effectiveSessionConfig (live /settings precedence, issue #20) ──────────

func TestEffectiveSessionConfigDefaultsWhenSettingsUntouched(t *testing.T) {
	m := &hlsManager{cfg: DefaultHLSConfig(), settings: stubSettings{
		// Fresh-install /settings defaults (settings.makeDefaults): must NOT
		// change behaviour versus no /settings integration at all.
		"transcodeMaxBitRate":    0,
		"transcodeMaxWidth":      1920,
		"transcodeConcurrency":   1,
		"transcodeHardwareAccel": true,
		"transcodeProfile":       nil,
	}}
	sc := m.effectiveSessionConfig()
	if sc.videoBitrate != "8M" || sc.videoMaxrate != "8M" || sc.videoBufsize != "16M" {
		t.Errorf("untouched transcodeMaxBitRate changed bitrate config: %+v", sc)
	}
	if sc.maxrateBps != legacyMaxrateBps {
		t.Errorf("maxrateBps = %d, want legacy default %d", sc.maxrateBps, legacyMaxrateBps)
	}
	if sc.maxWidth != 0 {
		t.Errorf("untouched transcodeMaxWidth (schema default 1920) must not enable downscale, got MaxWidth=%d", sc.maxWidth)
	}
	if !sc.hwEnabled {
		t.Error("transcodeHardwareAccel=true (default) must preserve auto hw behaviour (hwEnabled=true)")
	}
	if sc.x264Preset != "veryfast" || sc.nvencPreset != "p4" {
		t.Errorf("untouched transcodeProfile changed preset config: x264=%q nvenc=%q", sc.x264Preset, sc.nvencPreset)
	}
	// DefaultHLSConfig() (used unnormalized here) leaves SegmentConcurrency
	// at its 0 sentinel; currentConcurrency() floors that to 1 regardless —
	// the untouched transcodeConcurrency=1 schema default must not be
	// mistaken for an active override on top of that floor.
	if got := m.currentConcurrency(); got != 1 {
		t.Errorf("untouched transcodeConcurrency (schema default 1) must not override: got %d, want 1", got)
	}
}

func TestEffectiveSessionConfigAppliesExplicitOverrides(t *testing.T) {
	m := &hlsManager{cfg: DefaultHLSConfig(), settings: stubSettings{
		"transcodeMaxBitRate":    float64(3_000_000), // json.Unmarshal shape
		"transcodeMaxWidth":      float64(1280),
		"transcodeConcurrency":   float64(4),
		"transcodeHardwareAccel": false,
		"transcodeProfile":       "slow",
	}}
	sc := m.effectiveSessionConfig()
	if sc.maxrateBps != 3_000_000 {
		t.Errorf("maxrateBps = %d, want 3000000", sc.maxrateBps)
	}
	if sc.videoBitrate != "3000000" || sc.videoMaxrate != "3000000" {
		t.Errorf("videoBitrate/videoMaxrate = %q/%q, want the maxBitRate value", sc.videoBitrate, sc.videoMaxrate)
	}
	if sc.videoBufsize != "6000000" {
		t.Errorf("videoBufsize = %q, want 2x maxrate (6000000)", sc.videoBufsize)
	}
	if sc.maxWidth != 1280 {
		t.Errorf("maxWidth = %d, want 1280 (explicit non-default override)", sc.maxWidth)
	}
	if sc.hwEnabled {
		t.Error("transcodeHardwareAccel=false must disable hw for this session")
	}
	if sc.x264Preset != "slow" || sc.nvencPreset != "p6" {
		t.Errorf("transcodeProfile=slow mapping wrong: x264=%q nvenc=%q", sc.x264Preset, sc.nvencPreset)
	}
	if got := m.currentConcurrency(); got != 4 {
		t.Errorf("currentConcurrency() = %d, want 4 (explicit transcodeConcurrency override)", got)
	}
}

func TestEffectiveSessionConfigNilSettings(t *testing.T) {
	m := &hlsManager{cfg: DefaultHLSConfig(), settings: nil}
	sc := m.effectiveSessionConfig()
	if sc.maxrateBps != legacyMaxrateBps || sc.maxWidth != 0 || !sc.hwEnabled {
		t.Errorf("nil settings must behave exactly like env-only defaults, got %+v", sc)
	}
}

func TestEffectiveSessionConfigUnknownProfileIgnored(t *testing.T) {
	m := &hlsManager{cfg: DefaultHLSConfig(), settings: stubSettings{"transcodeProfile": "turbo-mode"}}
	sc := m.effectiveSessionConfig()
	if sc.x264Preset != "veryfast" || sc.nvencPreset != "p4" {
		t.Errorf("unrecognized transcodeProfile must be ignored, got x264=%q nvenc=%q", sc.x264Preset, sc.nvencPreset)
	}
}

// ── currentConcurrency / acquireTranscodeSlot ───────────────────────────────

func TestAcquireTranscodeSlotBoundsConcurrency(t *testing.T) {
	m := &hlsManager{cfg: HLSConfig{SegmentConcurrency: 2}}
	ctx := context.Background()

	if err := m.acquireTranscodeSlot(ctx); err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	if err := m.acquireTranscodeSlot(ctx); err != nil {
		t.Fatalf("acquire 2: %v", err)
	}

	// Third acquire must block until a slot is released; verify it does not
	// return immediately, then release and confirm it unblocks.
	acquired := make(chan error, 1)
	go func() { acquired <- m.acquireTranscodeSlot(ctx) }()
	select {
	case <-acquired:
		t.Fatal("acquireTranscodeSlot returned before a slot was released")
	case <-time.After(80 * time.Millisecond):
	}
	m.releaseTranscodeSlot()
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("acquire 3 after release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acquireTranscodeSlot did not unblock after release")
	}
	m.releaseTranscodeSlot()
	m.releaseTranscodeSlot()
}

func TestAcquireTranscodeSlotRespectsContextCancellation(t *testing.T) {
	m := &hlsManager{cfg: HLSConfig{SegmentConcurrency: 1}}
	if err := m.acquireTranscodeSlot(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer m.releaseTranscodeSlot()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := m.acquireTranscodeSlot(ctx); err == nil {
		t.Fatal("acquireTranscodeSlot should have returned an error when ctx expired while blocked")
	}
}

func TestCurrentConcurrencyLiveOverride(t *testing.T) {
	m := &hlsManager{cfg: HLSConfig{SegmentConcurrency: 3}, settings: stubSettings{"transcodeConcurrency": float64(1)}}
	if got := m.currentConcurrency(); got != 3 {
		t.Errorf("transcodeConcurrency=1 (schema default) must not override: got %d, want 3", got)
	}
	// Live change, applied without reconstructing the manager.
	m.settings = stubSettings{"transcodeConcurrency": float64(8)}
	if got := m.currentConcurrency(); got != 8 {
		t.Errorf("currentConcurrency did not pick up the live /settings change: got %d, want 8", got)
	}
}

// ── newHLSBaseDir work dir option ───────────────────────────────────────────

func TestNewHLSBaseDirWorkDirOption(t *testing.T) {
	root := t.TempDir()
	sub := root + "/hls-work"
	base := newHLSBaseDir(sub)
	defer func() { _ = os.RemoveAll(base) }()

	if !strings.HasPrefix(base, sub) {
		t.Errorf("newHLSBaseDir(%q) = %q, want a path rooted under workDir", sub, base)
	}
	if !strings.Contains(base, "stremio-hls-") {
		t.Errorf("newHLSBaseDir(%q) = %q, want a stremio-hls-* leaf name", sub, base)
	}
}

func TestNewHLSBaseDirEmptyWorkDirUsesOSDefault(t *testing.T) {
	base := newHLSBaseDir("")
	defer func() { _ = os.RemoveAll(base) }()
	if base == "" {
		t.Fatal("newHLSBaseDir(\"\") returned an empty path")
	}
}

// ── buildMasterPlaylist golden tests ────────────────────────────────────────

func TestBuildMasterPlaylistGoldenDefaults(t *testing.T) {
	// Reproduces the exact strings hls.go emitted before issue #20's
	// derivation existed (bandwidth=4000000, avc1.640029 Level 4.1),
	// regardless of caller-supplied streams.
	sc := sessionConfig{maxrateBps: legacyMaxrateBps}
	bw, codecs := deriveBandwidthCodecs(sc, 0, 0, false)

	t.Run("single audio, no subs", func(t *testing.T) {
		got := buildMasterPlaylist(masterPlaylistInputs{bandwidth: bw, codecsVideo: codecs})
		want := "#EXTM3U\n#EXT-X-VERSION:4\n#EXT-X-STREAM-INF:BANDWIDTH=4000000,CODECS=\"avc1.640029,mp4a.40.2\"\nplaylist.m3u8\n"
		if got != want {
			t.Errorf("got:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("single audio, with subs", func(t *testing.T) {
		got := buildMasterPlaylist(masterPlaylistInputs{
			bandwidth: bw, codecsVideo: codecs,
			subtitleStreams: []subtitleStream{{Language: "eng", Title: "English"}},
		})
		if !strings.Contains(got, "#EXT-X-STREAM-INF:BANDWIDTH=4000000,CODECS=\"avc1.640029,mp4a.40.2\",SUBTITLES=\"subs\"\nplaylist.m3u8\n") {
			t.Errorf("missing expected single-audio+subs STREAM-INF line:\n%s", got)
		}
	})

	t.Run("multi audio, no subs", func(t *testing.T) {
		got := buildMasterPlaylist(masterPlaylistInputs{
			bandwidth: bw, codecsVideo: codecs, multiAudio: true,
			audioStreams: []audioStream{{Language: "eng"}, {Language: "spa"}},
		})
		if !strings.Contains(got, "#EXT-X-STREAM-INF:BANDWIDTH=4000000,CODECS=\"avc1.640029,mp4a.40.2\",AUDIO=\"aud\"\nvideo.m3u8\n") {
			t.Errorf("missing expected multi-audio STREAM-INF line:\n%s", got)
		}
	})

	t.Run("multi audio, with subs", func(t *testing.T) {
		got := buildMasterPlaylist(masterPlaylistInputs{
			bandwidth: bw, codecsVideo: codecs, multiAudio: true,
			audioStreams:    []audioStream{{Language: "eng"}, {Language: "spa"}},
			subtitleStreams: []subtitleStream{{Language: "eng"}},
		})
		if !strings.Contains(got, "#EXT-X-STREAM-INF:BANDWIDTH=4000000,CODECS=\"avc1.640029,mp4a.40.2\",AUDIO=\"aud\",SUBTITLES=\"subs\"\nvideo.m3u8\n") {
			t.Errorf("missing expected multi-audio+subs STREAM-INF line:\n%s", got)
		}
	})
}

func TestBuildMasterPlaylistDerivedForNonDefaultConfig(t *testing.T) {
	// A server with a customized bitrate cap and an active downscale must
	// advertise a truthful, derived BANDWIDTH/CODECS instead of the legacy
	// fixed values.
	sc := sessionConfig{maxrateBps: 2_000_000}
	bw, codecs := deriveBandwidthCodecs(sc, 1280, 720, true)
	if bw != 1_000_000 || codecs != "avc1.64001f" {
		t.Fatalf("derivation itself wrong: bw=%d codecs=%q", bw, codecs)
	}
	got := buildMasterPlaylist(masterPlaylistInputs{bandwidth: bw, codecsVideo: codecs})
	want := "#EXTM3U\n#EXT-X-VERSION:4\n#EXT-X-STREAM-INF:BANDWIDTH=1000000,CODECS=\"avc1.64001f,mp4a.40.2\"\nplaylist.m3u8\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// ── settings* helpers ────────────────────────────────────────────────────────

func TestSettingsHelpersHandleEveryJSONShape(t *testing.T) {
	ss := stubSettings{
		"i":   42,
		"f":   float64(42),
		"i64": int64(42),
		"b":   true,
		"s":   "hello",
		"n":   nil,
	}
	if v, ok := settingsInt(ss, "i"); !ok || v != 42 {
		t.Errorf("settingsInt(int) = %d,%v", v, ok)
	}
	if v, ok := settingsInt(ss, "f"); !ok || v != 42 {
		t.Errorf("settingsInt(float64) = %d,%v", v, ok)
	}
	if v, ok := settingsInt(ss, "i64"); !ok || v != 42 {
		t.Errorf("settingsInt(int64) = %d,%v", v, ok)
	}
	if _, ok := settingsInt(ss, "missing"); ok {
		t.Error("settingsInt(missing) should be !ok")
	}
	if v, ok := settingsBool(ss, "b"); !ok || !v {
		t.Errorf("settingsBool = %v,%v", v, ok)
	}
	if v, ok := settingsString(ss, "s"); !ok || v != "hello" {
		t.Errorf("settingsString = %q,%v", v, ok)
	}
	if _, ok := settingsString(ss, "n"); ok {
		t.Error("settingsString(nil) should be !ok")
	}
	if _, ok := settingsInt(nil, "i"); ok {
		t.Error("settingsInt(nil source) should be !ok")
	}
}
