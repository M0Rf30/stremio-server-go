// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package media

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/M0Rf30/stremio-server-go/internal/logging"
)

// HDR→SDR tone mapping for HLS transcodes (STREMIO_TRANSCODE_TONEMAP).
//
// Without it, HDR10/HDR10+/HLG sources only get a pixel-format conversion
// (format=yuv420p / format=nv12), which keeps the PQ/HLG transfer curve and
// BT.2020 primaries in an output every player treats as SDR BT.709: the
// picture looks washed out and grey. When enabled, HDR sources are routed
// through a software zscale+tonemap chain instead (see tonemapChain); SDR
// sources, and every source when the knob is off, keep the exact filter
// chain and arguments they had before.

// TonemapAlgorithms lists the ffmpeg tonemap filter algorithms accepted for
// HLSConfig.Tonemap (the filter's own option values, minus "none").
var TonemapAlgorithms = []string{"hable", "mobius", "reinhard", "clip", "linear", "gamma"}

// IsTonemapAlgorithm reports whether s is one of TonemapAlgorithms.
func IsTonemapAlgorithm(s string) bool {
	for _, a := range TonemapAlgorithms {
		if a == s {
			return true
		}
	}
	return false
}

// videoColor is the colour metadata ffprobe reports for the first video
// stream, plus the Dolby Vision configuration record when present.
type videoColor struct {
	transfer   string // color_transfer, e.g. "smpte2084" (PQ), "arib-std-b67" (HLG), "bt709"
	primaries  string // color_primaries, e.g. "bt2020"
	matrix     string // color_space, e.g. "bt2020nc"
	colorRange string // color_range: "tv" (limited) or "pc" (full)

	// Dolby Vision configuration record (stream side data "DOVI
	// configuration record"); hasDOVI is false when the stream carries none.
	hasDOVI      bool
	doviProfile  int
	doviBLCompat int // dv_bl_signal_compatibility_id: 0 = none, 1 = HDR10, 2 = SDR, 4 = HLG, 6 = BD HDR10
}

// isHDR reports whether the stream uses an HDR transfer function: SMPTE
// ST 2084 (PQ; HDR10/HDR10+ and most Dolby Vision base layers) or ARIB
// STD-B67 (HLG). Bit depth alone is deliberately not used: 10-bit SDR
// (e.g. Hi10P anime) exists and must not be tone mapped.
func (c videoColor) isHDR() bool {
	return c.transfer == "smpte2084" || c.transfer == "arib-std-b67"
}

// doviWithoutCompatibleBase reports a Dolby Vision stream whose base layer
// is not an HDR10/HLG/SDR signal on its own — profile 5 (IPTPQc2), signalled
// by bl_signal_compatibility_id 0. Such a picture is only correct after
// applying the DV RPU reshaping, which the zscale/tonemap chain cannot do,
// so it is left untouched. Profiles 7/8.1 (HDR10 base) and 8.4 (HLG base)
// are tone mapped through the ordinary PQ/HLG path.
func (c videoColor) doviWithoutCompatibleBase() bool {
	return c.hasDOVI && (c.doviProfile == 5 || c.doviBLCompat == 0)
}

// tonemapPlan is the per-segment tone-mapping decision: the algorithm to use
// and the source colour metadata it converts from. The zero value means "no
// tone mapping" and leaves every filter chain exactly as before.
type tonemapPlan struct {
	algo  string
	color videoColor
	// trimStart is transcodeSegment's output-seek residual (seconds). When
	// > 0 the chain starts with trim=start=<trimStart>, dropping the
	// hybrid-seek pre-roll frames before they are tone mapped. ffmpeg
	// implements the output -ss as a trim at the *end* of the filter graph,
	// so without this every segment would also tone map up to 10 s of frames
	// that are then discarded; the trim uses the same timeline and value as
	// that output trim, so the encoded frames are identical.
	trimStart float64
}

func (p tonemapPlan) enabled() bool { return p.algo != "" }

// planTonemap decides whether a source with colour metadata c is tone mapped
// with algo (the manager's capability-resolved algorithm; "" = off).
func planTonemap(algo string, c videoColor) tonemapPlan {
	if algo == "" || !c.isHDR() || c.doviWithoutCompatibleBase() {
		return tonemapPlan{}
	}
	return tonemapPlan{algo: algo, color: c}
}

// zscaleOr returns v when it is one of allowed, else def. Probed metadata is
// only ever interpolated into the filter graph through this whitelist.
func zscaleOr(v, def string, allowed ...string) string {
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	return def
}

// tonemapChain builds the software HDR→SDR filter chain, ending in 8-bit-ready
// BT.709 limited-range video (the caller appends the backend's own
// format=yuv420p or format=nv12,hwupload):
//
//  0. trim (only when p.trimStart > 0): drop the seek pre-roll first.
//  1. zscale: linearise the PQ/HLG signal (input transfer/primaries/matrix/
//     range stated explicitly from the probe, falling back to BT.2020
//     limited range for missing or unexpected tags, and output primaries
//     pinned to the same value, so untagged frames still convert), with
//     npl=100 so 1.0 = 100 nits (SDR reference white); when
//     a downscale is configured it happens here too, so the float stages
//     below run at output resolution (~4x cheaper for 4K→1080p).
//  2. format=gbrpf32le: float RGB, what the tonemap filter works on.
//  3. zscale=p=bt709: BT.2020 → BT.709 primaries, in linear light.
//  4. tonemap: compress the HDR highlights into SDR range; desat=0 keeps
//     highlight colour instead of fading it to white.
//  5. zscale: apply the BT.709 transfer and matrix, limited range.
func tonemapChain(p tonemapPlan, w, h int, scaled bool) string {
	c := p.color
	first := "zscale="
	if scaled {
		first += fmt.Sprintf("w=%d:h=%d:f=bicubic:", w, h)
	}
	// The output primaries (p=) are pinned to the same value as the input
	// (pin=): zscale otherwise takes them from the frame's own tag, and an
	// untagged ("unspecified") frame then fails with "no path between
	// colorspaces". The actual primaries conversion happens in step 3.
	prim := zscaleOr(c.primaries, "bt2020", "bt2020", "bt709", "smpte432")
	first += "tin=" + c.transfer +
		":pin=" + prim + ":p=" + prim +
		":min=" + zscaleOr(c.matrix, "bt2020nc", "bt2020nc", "bt2020c", "bt709") +
		":rin=" + zscaleOr(c.colorRange, "tv", "tv", "pc") +
		":t=linear:npl=100"
	var chain []string
	if p.trimStart > 0 {
		chain = append(chain, "trim=start="+strconv.FormatFloat(p.trimStart, 'f', 3, 64))
	}
	chain = append(chain,
		first,
		"format=gbrpf32le",
		"zscale=p=bt709",
		"tonemap=tonemap="+p.algo+":desat=0",
		"zscale=t=bt709:m=bt709:r=tv",
	)
	return strings.Join(chain, ",")
}

// tonemapOutputTags are appended to the video encoder arguments whenever
// tone mapping ran, so players don't re-interpret the SDR output as HDR.
// The :v stream specifier keeps them off the audio encoder in muxed segments.
var tonemapOutputTags = []string{"-color_primaries:v", "bt709", "-color_trc:v", "bt709", "-colorspace:v", "bt709"}

// ── filter capability detection ─────────────────────────────────────────────

// filtListOnce guards the one-time run of `ffmpeg -hide_banner -filters`.
var (
	filtListOnce sync.Once
	filtListOut  string
)

// filtersList returns the cached stdout of `ffmpeg -hide_banner -filters`.
// Like encodersList, the command runs at most once per process; it is only
// invoked at all when tone mapping was requested.
func filtersList() string {
	filtListOnce.Do(func() {
		out, err := exec.Command("ffmpeg", "-hide_banner", "-filters").Output()
		if err == nil {
			filtListOut = string(out)
		}
	})
	return filtListOut
}

// hasFilter reports whether the `ffmpeg -filters` listing contains a filter
// named exactly name (e.g. "tonemap" must not match "tonemap_opencl"). Each
// entry line is "<flags> <name> <in>-><out> <description>".
func hasFilter(list, name string) bool {
	for _, line := range strings.Split(list, "\n") {
		if f := strings.Fields(line); len(f) >= 3 && f[1] == name && strings.Contains(f[2], "->") {
			return true
		}
	}
	return false
}

// tonemapRequiredFilters are the ffmpeg filters tonemapChain needs. zscale
// is only built when ffmpeg is linked against libzimg (--enable-libzimg).
var tonemapRequiredFilters = []string{"zscale", "tonemap"}

// resolveTonemap returns requested when the ffmpeg filter listing (filters,
// normally filtersList()) contains every filter tonemapChain needs, and ""
// (tone mapping off, today's behaviour) otherwise, logging one warning.
func resolveTonemap(requested, filters string) string {
	if requested == "" {
		return ""
	}
	var missing []string
	for _, f := range tonemapRequiredFilters {
		if !hasFilter(filters, f) {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		logging.For("media").Warn("HDR tone mapping disabled: ffmpeg lacks required filters",
			"algorithm", requested, "missing", strings.Join(missing, ","),
			"hint", "use an ffmpeg build with libzimg (zscale)")
		return ""
	}
	logging.For("media").Info("HDR to SDR tone mapping enabled for HLS transcodes", "algorithm", requested)
	return requested
}
