// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package media

// Tests for internal/media/tonemap.go and its integration points: HDR
// detection from ffprobe JSON, the tone-mapping decision, the zscale/tonemap
// filter chain per encoder backend, the capability fallback, and — most
// importantly — that SDR sources and a disabled knob keep producing exactly
// the arguments they did before tone mapping existed.

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// ── HDR detection ───────────────────────────────────────────────────────────

func TestParseProbeOutputColorAndHDRDetection(t *testing.T) {
	stream := func(extra string) string {
		return `{"format":{"duration":"60.0"},"streams":[{"index":0,"codec_type":"video","codec_name":"hevc",` +
			`"pix_fmt":"yuv420p10le","profile":"Main 10","width":3840,"height":2160` + extra + `}]}`
	}
	cases := []struct {
		name      string
		json      string
		wantColor videoColor
		wantHDR   bool
		wantDVNo  bool // doviWithoutCompatibleBase
	}{
		{
			name: "HDR10 PQ",
			json: stream(`,"color_range":"tv","color_space":"bt2020nc","color_transfer":"smpte2084","color_primaries":"bt2020"`),
			wantColor: videoColor{
				transfer: "smpte2084", primaries: "bt2020", matrix: "bt2020nc", colorRange: "tv",
			},
			wantHDR: true,
		},
		{
			name: "HLG",
			json: stream(`,"color_range":"tv","color_space":"bt2020nc","color_transfer":"arib-std-b67","color_primaries":"bt2020"`),
			wantColor: videoColor{
				transfer: "arib-std-b67", primaries: "bt2020", matrix: "bt2020nc", colorRange: "tv",
			},
			wantHDR: true,
		},
		{
			name: "SDR 10-bit BT.709 is not HDR",
			json: stream(`,"color_range":"tv","color_space":"bt709","color_transfer":"bt709","color_primaries":"bt709"`),
			wantColor: videoColor{
				transfer: "bt709", primaries: "bt709", matrix: "bt709", colorRange: "tv",
			},
		},
		{
			name: "SDR 10-bit BT.2020 (bt2020-10 transfer) is not HDR",
			json: stream(`,"color_space":"bt2020nc","color_transfer":"bt2020-10","color_primaries":"bt2020"`),
			wantColor: videoColor{
				transfer: "bt2020-10", primaries: "bt2020", matrix: "bt2020nc",
			},
		},
		{
			name:      "missing colour fields",
			json:      stream(``),
			wantColor: videoColor{},
		},
		{
			name: "Dolby Vision profile 8.1 (HDR10 base layer) stays HDR10",
			json: stream(`,"color_space":"bt2020nc","color_transfer":"smpte2084","color_primaries":"bt2020",` +
				`"side_data_list":[{"side_data_type":"DOVI configuration record","dv_version_major":1,"dv_version_minor":0,` +
				`"dv_profile":8,"dv_level":6,"rpu_present_flag":1,"el_present_flag":0,"bl_present_flag":1,"dv_bl_signal_compatibility_id":1}]`),
			wantColor: videoColor{
				transfer: "smpte2084", primaries: "bt2020", matrix: "bt2020nc",
				hasDOVI: true, doviProfile: 8, doviBLCompat: 1,
			},
			wantHDR: true,
		},
		{
			name: "Dolby Vision profile 5 (no compatible base layer)",
			json: stream(`,"side_data_list":[{"side_data_type":"DOVI configuration record","dv_version_major":1,"dv_version_minor":0,` +
				`"dv_profile":5,"dv_level":6,"rpu_present_flag":1,"el_present_flag":0,"bl_present_flag":1,"dv_bl_signal_compatibility_id":0}]`),
			wantColor: videoColor{hasDOVI: true, doviProfile: 5, doviBLCompat: 0},
			wantDVNo:  true,
		},
		{
			name: "Dolby Vision profile 5 even if mis-tagged PQ",
			json: stream(`,"color_transfer":"smpte2084",` +
				`"side_data_list":[{"side_data_type":"DOVI configuration record","dv_profile":5,"dv_bl_signal_compatibility_id":0}]`),
			wantColor: videoColor{transfer: "smpte2084", hasDOVI: true, doviProfile: 5},
			wantHDR:   true,
			wantDVNo:  true,
		},
		{
			name: "only the first video stream counts",
			json: `{"format":{"duration":"60.0"},"streams":[` +
				`{"index":0,"codec_type":"video","color_transfer":"bt709","width":1920,"height":1080},` +
				`{"index":1,"codec_type":"video","color_transfer":"smpte2084","width":640,"height":360}]}`,
			wantColor: videoColor{transfer: "bt709"},
		},
		{
			name:      "undecodable output",
			json:      `not json`,
			wantColor: videoColor{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := parseProbeOutput([]byte(c.json))
			if res.color != c.wantColor {
				t.Errorf("color = %+v, want %+v", res.color, c.wantColor)
			}
			if got := res.color.isHDR(); got != c.wantHDR {
				t.Errorf("isHDR() = %v, want %v", got, c.wantHDR)
			}
			if got := res.color.doviWithoutCompatibleBase(); got != c.wantDVNo {
				t.Errorf("doviWithoutCompatibleBase() = %v, want %v", got, c.wantDVNo)
			}
		})
	}
}

func TestParseProbeOutputLenientSideData(t *testing.T) {
	// A malformed side-data entry (wrong field type) must not zero the whole
	// probe; a valid DOVI record next to it is still picked up.
	res := parseProbeOutput([]byte(`{"format":{"duration":"60.0"},"streams":[{"index":0,"codec_type":"video",` +
		`"width":3840,"height":2160,"color_transfer":"smpte2084","side_data_list":[` +
		`{"side_data_type":"DOVI configuration record","dv_profile":"eight"},` +
		`{"side_data_type":"Mastering display metadata","max_luminance":"10000000/10000"},` +
		`{"side_data_type":"DOVI configuration record","dv_profile":8,"dv_bl_signal_compatibility_id":1}]}]}`))
	if res.duration != 60 || res.width != 3840 {
		t.Fatalf("probe zeroed by malformed side data: %+v", res)
	}
	want := videoColor{transfer: "smpte2084", hasDOVI: true, doviProfile: 8, doviBLCompat: 1}
	if res.color != want {
		t.Errorf("color = %+v, want %+v", res.color, want)
	}
}

func TestParseProbeOutputKeepsExistingFields(t *testing.T) {
	// The probe refactor (parseProbeOutput split out of probeMedia) must not
	// change any pre-existing result field.
	res := parseProbeOutput([]byte(`{"format":{"duration":"12.5"},"streams":[` +
		`{"index":0,"codec_type":"video","pix_fmt":"yuv420p10le","width":3840,"height":2160,"color_transfer":"smpte2084"},` +
		`{"index":1,"codec_type":"audio","codec_name":"eac3","channels":6,"tags":{"language":"eng"},"disposition":{"default":1}},` +
		`{"index":2,"codec_type":"subtitle","codec_name":"subrip","tags":{"language":"ita"}}]}`))
	if res.duration != 12.5 || !res.highBitDepth || res.width != 3840 || res.height != 2160 {
		t.Errorf("duration/highBitDepth/dims = %v/%v/%dx%d", res.duration, res.highBitDepth, res.width, res.height)
	}
	if len(res.audioStreams) != 1 || res.audioStreams[0].Channels != 6 || !res.audioStreams[0].IsDefault {
		t.Errorf("audioStreams = %+v", res.audioStreams)
	}
	if len(res.subtitleStreams) != 1 || res.subtitleStreams[0].Language != "ita" {
		t.Errorf("subtitleStreams = %+v", res.subtitleStreams)
	}
}

// ── decision + chain ────────────────────────────────────────────────────────

var (
	colorPQ  = videoColor{transfer: "smpte2084", primaries: "bt2020", matrix: "bt2020nc", colorRange: "tv"}
	colorHLG = videoColor{transfer: "arib-std-b67", primaries: "bt2020", matrix: "bt2020nc", colorRange: "tv"}
	colorSDR = videoColor{transfer: "bt709", primaries: "bt709", matrix: "bt709", colorRange: "tv"}
	colorDV5 = videoColor{transfer: "smpte2084", hasDOVI: true, doviProfile: 5}
	colorDV8 = videoColor{transfer: "smpte2084", primaries: "bt2020", matrix: "bt2020nc", hasDOVI: true, doviProfile: 8, doviBLCompat: 1}
)

func TestPlanTonemap(t *testing.T) {
	cases := []struct {
		name  string
		algo  string
		color videoColor
		want  bool
	}{
		{"off, PQ", "", colorPQ, false},
		{"on, PQ", "hable", colorPQ, true},
		{"on, HLG", "mobius", colorHLG, true},
		{"on, SDR", "hable", colorSDR, false},
		{"on, untagged", "hable", videoColor{}, false},
		{"on, DV profile 5", "hable", colorDV5, false},
		{"on, DV profile 8.1", "hable", colorDV8, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := planTonemap(c.algo, c.color)
			if p.enabled() != c.want {
				t.Fatalf("planTonemap(%q, %+v).enabled() = %v, want %v", c.algo, c.color, p.enabled(), c.want)
			}
			if !c.want && p != (tonemapPlan{}) {
				t.Errorf("disabled plan must be the zero value, got %+v", p)
			}
		})
	}
}

func TestTonemapChain(t *testing.T) {
	cases := []struct {
		name   string
		plan   tonemapPlan
		scaled bool
		want   string
	}{
		{
			"PQ unscaled", tonemapPlan{"hable", colorPQ, 0}, false,
			"zscale=tin=smpte2084:pin=bt2020:p=bt2020:min=bt2020nc:rin=tv:t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv",
		},
		{
			"PQ scaled: resize in the first zscale", tonemapPlan{"hable", colorPQ, 0}, true,
			"zscale=w=1920:h=1080:f=bicubic:tin=smpte2084:pin=bt2020:p=bt2020:min=bt2020nc:rin=tv:t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv",
		},
		{
			"pre-roll trim comes first", tonemapPlan{"hable", colorPQ, 6.5}, true,
			"trim=start=6.500,zscale=w=1920:h=1080:f=bicubic:tin=smpte2084:pin=bt2020:p=bt2020:min=bt2020nc:rin=tv:t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv",
		},
		{
			"HLG with mobius", tonemapPlan{"mobius", colorHLG, 0}, false,
			"zscale=tin=arib-std-b67:pin=bt2020:p=bt2020:min=bt2020nc:rin=tv:t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=mobius:desat=0,zscale=t=bt709:m=bt709:r=tv",
		},
		{
			"missing/unknown primaries, matrix, range fall back to BT.2020 limited",
			tonemapPlan{"reinhard", videoColor{transfer: "smpte2084", primaries: "weird:x=1", matrix: "", colorRange: "unknown"}, 0}, false,
			"zscale=tin=smpte2084:pin=bt2020:p=bt2020:min=bt2020nc:rin=tv:t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=reinhard:desat=0,zscale=t=bt709:m=bt709:r=tv",
		},
		{
			"primaries outside the whitelist (bt470bg) resolve to BT.2020 in and out",
			tonemapPlan{"mobius", videoColor{transfer: "smpte2084", primaries: "bt470bg", matrix: "bt2020nc", colorRange: "tv"}, 0}, false,
			"zscale=tin=smpte2084:pin=bt2020:p=bt2020:min=bt2020nc:rin=tv:t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=mobius:desat=0,zscale=t=bt709:m=bt709:r=tv",
		},
		{
			"full-range input", tonemapPlan{"clip", videoColor{transfer: "smpte2084", primaries: "bt2020", matrix: "bt2020c", colorRange: "pc"}, 0}, false,
			"zscale=tin=smpte2084:pin=bt2020:p=bt2020:min=bt2020c:rin=pc:t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=clip:desat=0,zscale=t=bt709:m=bt709:r=tv",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tonemapChain(c.plan, 1920, 1080, c.scaled); got != c.want {
				t.Errorf("tonemapChain =\n  %s\nwant\n  %s", got, c.want)
			}
		})
	}
}

// ── byte-identical args when tone mapping does not apply ──────────────────

// legacyBuildVideoFilter and legacyVideoEncodeArgs are code-identical copies
// (comments removed) of the video filter/encoder argument code as it was
// before tone mapping was added
// (upstream 9475695: buildVideoFilter in config.go and the video switch in
// transcodeSegment). They pin today's behaviour so the tests below can assert
// that SDR sources and a disabled knob still produce byte-identical args.
func legacyBuildVideoFilter(codec string, needsFormatConv bool, scaledW, scaledH int, scaled bool) string {
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

func legacyVideoEncodeArgs(codec string, highBitDepth bool, outW, outH int, scaled bool, tc sessionConfig, cfg HLSConfig) []string {
	gopStr := strconv.Itoa(videoGOP)
	var a []string
	switch codec {
	case "h264_vaapi":
		a = append(a,
			"-vf", legacyBuildVideoFilter(codec, true, outW, outH, scaled),
			"-c:v", "h264_vaapi", "-qp", strconv.Itoa(cfg.VAAPIQP),
			"-g", gopStr,
			"-b:v", tc.videoBitrate, "-maxrate", tc.videoMaxrate, "-bufsize", tc.videoBufsize,
		)
	case "h264_nvenc":
		if vf := legacyBuildVideoFilter(codec, highBitDepth, outW, outH, scaled); vf != "" {
			a = append(a, "-vf", vf)
		}
		a = append(a,
			"-c:v", "h264_nvenc", "-preset", tc.nvencPreset, "-rc", "vbr",
			"-g", gopStr,
			"-b:v", tc.videoBitrate, "-maxrate", tc.videoMaxrate, "-bufsize", tc.videoBufsize,
		)
	case "h264_qsv":
		if vf := legacyBuildVideoFilter(codec, highBitDepth, outW, outH, scaled); vf != "" {
			a = append(a, "-vf", vf)
		}
		a = append(a,
			"-c:v", "h264_qsv", "-preset", "veryfast",
			"-g", gopStr,
			"-b:v", tc.videoBitrate, "-maxrate", tc.videoMaxrate, "-bufsize", tc.videoBufsize,
		)
	case "h264_videotoolbox":
		if vf := legacyBuildVideoFilter(codec, highBitDepth, outW, outH, scaled); vf != "" {
			a = append(a, "-vf", vf)
		}
		a = append(a,
			"-c:v", "h264_videotoolbox", "-realtime", "1",
			"-g", gopStr,
			"-b:v", tc.videoBitrate, "-maxrate", tc.videoMaxrate, "-bufsize", tc.videoBufsize,
		)
	case "h264_v4l2m2m":
		if vf := legacyBuildVideoFilter(codec, highBitDepth, outW, outH, scaled); vf != "" {
			a = append(a, "-vf", vf)
		}
		a = append(a,
			"-c:v", "h264_v4l2m2m",
			"-g", gopStr,
			"-b:v", tc.videoBitrate, "-maxrate", tc.videoMaxrate, "-bufsize", tc.videoBufsize,
		)
	default:
		a = append(a,
			"-vf", legacyBuildVideoFilter("libx264", true, outW, outH, scaled),
			"-c:v", "libx264", "-preset", tc.x264Preset, "-crf", strconv.Itoa(cfg.X264CRF),
			"-profile:v", "high",
			"-sc_threshold", "0",
			"-g", gopStr, "-keyint_min", gopStr,
			"-b:v", tc.videoBitrate, "-maxrate", tc.videoMaxrate, "-bufsize", tc.videoBufsize,
		)
	}
	return a
}

var allEncoderCodecs = []string{"libx264", "h264_nvenc", "h264_qsv", "h264_vaapi", "h264_videotoolbox", "h264_v4l2m2m"}

func testSessionConfig() (sessionConfig, HLSConfig) {
	cfg := DefaultHLSConfig().normalize(1)
	m := &hlsManager{cfg: cfg}
	return m.effectiveSessionConfig(sessionOverrides{}), cfg
}

func TestVideoEncodeArgsByteIdenticalWithoutTonemap(t *testing.T) {
	tc, cfg := testSessionConfig()
	// Every encoder × scaled/unscaled × 8/10-bit × {knob off on an HDR
	// source, knob on/off on an SDR source, knob on for DV profile 5}.
	plans := []struct {
		name  string
		algo  string
		color videoColor
	}{
		{"tonemap off, PQ source", "", colorPQ},
		{"tonemap off, HLG source", "", colorHLG},
		{"tonemap off, SDR source", "", colorSDR},
		{"tonemap on, SDR source", "hable", colorSDR},
		{"tonemap on, untagged source", "hable", videoColor{}},
		{"tonemap on, DV profile 5", "hable", colorDV5},
	}
	for _, codec := range allEncoderCodecs {
		for _, scaled := range []bool{false, true} {
			for _, hbd := range []bool{false, true} {
				for _, p := range plans {
					name := fmt.Sprintf("%s/scaled=%v/10bit=%v/%s", codec, scaled, hbd, p.name)
					t.Run(name, func(t *testing.T) {
						tm := planTonemap(p.algo, p.color)
						got := videoEncodeArgs(codec, hbd, 1920, 1080, scaled, tc, cfg, tm)
						want := legacyVideoEncodeArgs(codec, hbd, 1920, 1080, scaled, tc, cfg)
						if !reflect.DeepEqual(got, want) {
							t.Errorf("args changed:\n got  %q\n want %q", got, want)
						}
					})
				}
			}
		}
	}
}

func TestVideoEncodeArgsDefaultGolden(t *testing.T) {
	// Literal goldens for the default configuration, independent of the
	// legacy copy above (so the equivalence test is not only self-confirming).
	tc, cfg := testSessionConfig()
	off := planTonemap("", colorPQ)
	cases := []struct {
		name   string
		codec  string
		hbd    bool
		scaled bool
		want   string
	}{
		{"libx264 10-bit unscaled", "libx264", true, false,
			"-vf format=yuv420p -c:v libx264 -preset veryfast -crf 23 -profile:v high -sc_threshold 0 -g 120 -keyint_min 120 -b:v 8M -maxrate 8M -bufsize 16M"},
		{"nvenc 8-bit unscaled (no -vf)", "h264_nvenc", false, false,
			"-c:v h264_nvenc -preset p4 -rc vbr -g 120 -b:v 8M -maxrate 8M -bufsize 16M"},
		{"nvenc 10-bit scaled", "h264_nvenc", true, true,
			"-vf format=yuv420p,scale=1920:1080 -c:v h264_nvenc -preset p4 -rc vbr -g 120 -b:v 8M -maxrate 8M -bufsize 16M"},
		{"qsv 10-bit unscaled", "h264_qsv", true, false,
			"-vf format=yuv420p -c:v h264_qsv -preset veryfast -g 120 -b:v 8M -maxrate 8M -bufsize 16M"},
		{"qsv 8-bit scaled", "h264_qsv", false, true,
			"-vf scale=1920:1080 -c:v h264_qsv -preset veryfast -g 120 -b:v 8M -maxrate 8M -bufsize 16M"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := strings.Join(videoEncodeArgs(c.codec, c.hbd, 1920, 1080, c.scaled, tc, cfg, off), " ")
			if got != c.want {
				t.Errorf("args =\n  %s\nwant\n  %s", got, c.want)
			}
		})
	}
}

// ── tone mapping on: per-encoder chains and output tags ───────────────────

func TestBuildVideoFilterTonemap(t *testing.T) {
	const chainPQ = "zscale=tin=smpte2084:pin=bt2020:p=bt2020:min=bt2020nc:rin=tv:t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv"
	const chainPQScaled = "zscale=w=1920:h=1080:f=bicubic:tin=smpte2084:pin=bt2020:p=bt2020:min=bt2020nc:rin=tv:t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv"
	tm := planTonemap("hable", colorPQ)
	for _, codec := range allEncoderCodecs {
		for _, scaled := range []bool{false, true} {
			for _, hbd := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/scaled=%v/10bit=%v", codec, scaled, hbd), func(t *testing.T) {
					chain := chainPQ
					if scaled {
						chain = chainPQScaled
					}
					want := chain + ",format=yuv420p"
					if codec == "h264_vaapi" {
						// Already scaled by zscale: no scale_vaapi after upload.
						want = chain + ",format=nv12,hwupload"
					}
					got := buildVideoFilter(codec, hbd, 1920, 1080, scaled, tm)
					if got != want {
						t.Errorf("buildVideoFilter =\n  %s\nwant\n  %s", got, want)
					}
					if strings.Contains(got, "scale=1920") || strings.Contains(got, "scale_vaapi") {
						t.Errorf("tone-mapped chain must not scale twice: %s", got)
					}
				})
			}
		}
	}
}

func TestVideoEncodeArgsTonemapOn(t *testing.T) {
	tc, cfg := testSessionConfig()
	for _, codec := range allEncoderCodecs {
		for _, color := range []videoColor{colorPQ, colorHLG, colorDV8} {
			t.Run(codec+"/"+color.transfer, func(t *testing.T) {
				tm := planTonemap("hable", color)
				got := videoEncodeArgs(codec, true, 1920, 1080, true, tc, cfg, tm)
				// Output colour tags appended, exactly once, at the end.
				n := len(tonemapOutputTags)
				if len(got) < n || !reflect.DeepEqual(got[len(got)-n:], tonemapOutputTags) {
					t.Errorf("missing BT.709 output tags at end: %q", got)
				}
				if strings.Count(strings.Join(got, " "), "-color_trc:v") != 1 {
					t.Errorf("color tags must appear once: %q", got)
				}
				// -vf carries the tone-mapping chain.
				vf := ""
				for i := 0; i+1 < len(got); i++ {
					if got[i] == "-vf" {
						vf = got[i+1]
					}
				}
				if !strings.HasPrefix(vf, "zscale=w=1920:h=1080:f=bicubic:tin="+color.transfer+":") ||
					!strings.Contains(vf, "tonemap=tonemap=hable") {
					t.Errorf("-vf = %q, want tone-mapping chain", vf)
				}
				// Everything except -vf's value and the tags matches the legacy
				// encoder args, so tone mapping changes nothing else.
				legacy := legacyVideoEncodeArgs(codec, true, 1920, 1080, true, tc, cfg)
				trimmed := append([]string(nil), got[:len(got)-n]...)
				for i := 0; i+1 < len(trimmed); i++ {
					if trimmed[i] == "-vf" {
						trimmed = append(trimmed[:i], trimmed[i+2:]...)
						break
					}
				}
				for i := 0; i+1 < len(legacy); i++ {
					if legacy[i] == "-vf" {
						legacy = append(legacy[:i], legacy[i+2:]...)
						break
					}
				}
				if !reflect.DeepEqual(trimmed, legacy) {
					t.Errorf("non-filter args changed:\n got  %q\n want %q", trimmed, legacy)
				}
			})
		}
	}
}

// ── capability detection / fallback ────────────────────────────────────────

// ffmpegFiltersFixture mimics `ffmpeg -hide_banner -filters` output.
func ffmpegFiltersFixture(names ...string) string {
	var b strings.Builder
	b.WriteString("Filters:\n  T.. = Timeline support\n  .S. = Slice threading\n  A = Audio input/output\n  V = Video input/output\n  ------\n")
	b.WriteString(" .. acopy             A->A       Copy the input audio unchanged to the output.\n")
	for _, n := range names {
		fmt.Fprintf(&b, " .S %-17s V->V       Some filter.\n", n)
	}
	return b.String()
}

func TestHasFilter(t *testing.T) {
	list := ffmpegFiltersFixture("tonemap_opencl", "tonemap_vaapi", "zscale")
	if !hasFilter(list, "zscale") {
		t.Error("zscale not found")
	}
	if hasFilter(list, "tonemap") {
		t.Error("tonemap must not match tonemap_opencl/tonemap_vaapi")
	}
	if hasFilter(list, "Timeline") || hasFilter(list, "=") {
		t.Error("legend lines must not match")
	}
	if hasFilter("", "zscale") {
		t.Error("empty listing (ffmpeg missing) must not match")
	}
}

func TestResolveTonemap(t *testing.T) {
	cases := []struct {
		name      string
		requested string
		filters   string
		want      string
	}{
		{"off stays off", "", ffmpegFiltersFixture("zscale", "tonemap"), ""},
		{"both filters present", "hable", ffmpegFiltersFixture("zscale", "tonemap"), "hable"},
		{"no zscale (no libzimg)", "hable", ffmpegFiltersFixture("tonemap"), ""},
		{"no tonemap", "mobius", ffmpegFiltersFixture("zscale", "tonemap_opencl"), ""},
		{"ffmpeg missing / listing failed", "hable", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveTonemap(c.requested, c.filters); got != c.want {
				t.Errorf("resolveTonemap(%q) = %q, want %q", c.requested, got, c.want)
			}
		})
	}
}

func TestCapabilityFallbackRestoresLegacyArgs(t *testing.T) {
	// When the filters are missing, the manager's resolved algorithm is ""
	// and an HDR source gets exactly today's arguments.
	tc, cfg := testSessionConfig()
	algo := resolveTonemap("hable", ffmpegFiltersFixture("tonemap"))
	for _, codec := range allEncoderCodecs {
		got := videoEncodeArgs(codec, true, 1920, 1080, true, tc, cfg, planTonemap(algo, colorPQ))
		want := legacyVideoEncodeArgs(codec, true, 1920, 1080, true, tc, cfg)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: fallback args changed:\n got  %q\n want %q", codec, got, want)
		}
	}
}

func TestHLSConfigNormalizeTonemap(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""}, {"hable", "hable"}, {"mobius", "mobius"}, {"bogus", ""}, {"HABLE", ""},
	} {
		cfg := DefaultHLSConfig()
		cfg.Tonemap = c.in
		if got := cfg.normalize(1).Tonemap; got != c.want {
			t.Errorf("normalize(Tonemap=%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestVideoEncodeArgsQSVPresetHonoredWithTonemap pins the PR28 x PR27
// interaction: after the video-arg switch was refactored into
// videoEncodeArgs (PR27), the h264_qsv case must still read the per-session
// QSV preset (PR28's STREMIO_TRANSCODE_QSV_PRESET / transcodeProfile
// mapping) instead of the pre-PR28 hardcoded "veryfast" — with tone mapping
// both off and on, since the tone-mapping filter chain is orthogonal to the
// encoder preset flag.
func TestVideoEncodeArgsQSVPresetHonoredWithTonemap(t *testing.T) {
	tc, cfg := testSessionConfig()
	tc.qsvPreset = "slow" // non-default, distinguishable from "veryfast"

	for _, tm := range []tonemapPlan{
		{},                            // off
		planTonemap("hable", colorPQ), // on, HDR source
	} {
		got := strings.Join(videoEncodeArgs("h264_qsv", true, 1920, 1080, false, tc, cfg, tm), " ")
		if !strings.Contains(got, "-preset slow") {
			t.Errorf("videoEncodeArgs(h264_qsv, tonemap=%v) = %q, want -preset slow (tc.qsvPreset honored)", tm.enabled(), got)
		}
		if strings.Contains(got, "-preset veryfast") {
			t.Errorf("videoEncodeArgs(h264_qsv, tonemap=%v) = %q, still hardcoded to veryfast", tm.enabled(), got)
		}
	}
}
