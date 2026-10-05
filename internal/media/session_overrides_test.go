// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package media

// Tests for the opt-in per-session HLS overrides on master.m3u8
// (STREMIO_HLS_SESSION_OVERRIDES): query parsing/validation, precedence over
// /settings and env, BANDWIDTH/CODECS derivation, the gate, existing-session
// behaviour, and per-session TTL eviction. StartHLS is driven without
// ffprobe by pre-seeding the manager's positive probe cache for the media
// URL, so everything stays offline.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/types"
)

// overrideMediaURL is a public IP literal: it passes validateRemoteURL with
// no DNS lookup, and is never actually fetched because the probe cache is
// pre-seeded for it.
const overrideMediaURL = "http://93.184.216.34/movie.mkv"

// newOverrideTestManager returns a reaper-less manager (see
// newTestHLSManager) with the gate set as requested, the given env config
// tweaks and /settings source, and a cached 1920x1080, 60s probe result for
// overrideMediaURL.
func newOverrideTestManager(t *testing.T, gate bool, cfg HLSConfig, ss SettingsSource) *hlsManager {
	t.Helper()
	cfg.SessionOverrides = gate
	m := &hlsManager{
		base:       t.TempDir(),
		cfg:        cfg.normalize(1),
		settings:   ss,
		sessions:   map[string]*hlsSession{},
		probeCache: map[string]probeCacheEntry{},
		stopCh:     make(chan struct{}),
	}
	m.probeCache[overrideMediaURL] = probeCacheEntry{
		result:    probeMediaResult{duration: 60, width: 1920, height: 1080},
		expiresAt: time.Now().Add(time.Hour),
	}
	return m
}

func session(t *testing.T, m *hlsManager, id string) *hlsSession {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		t.Fatalf("session %q not registered", id)
	}
	return s
}

// ── parseSessionOverrides ──────────────────────────────────────────────────

func TestParseSessionOverridesValid(t *testing.T) {
	cases := []struct {
		name string
		opts types.HLSSessionOptions
		want sessionOverrides
	}{
		{"nothing given", types.HLSSessionOptions{}, sessionOverrides{}},
		{"ttl whole seconds", types.HLSSessionOptions{TTL: "3600"}, sessionOverrides{ttl: time.Hour}},
		{"ttl duration string", types.HLSSessionOptions{TTL: "2h30m"}, sessionOverrides{ttl: 150 * time.Minute}},
		{"ttl exactly the 60s floor, seconds", types.HLSSessionOptions{TTL: "60"}, sessionOverrides{ttl: time.Minute}},
		{"ttl exactly the 60s floor, duration", types.HLSSessionOptions{TTL: "1m"}, sessionOverrides{ttl: time.Minute}},
		{"ttl exactly 30 days", types.HLSSessionOptions{TTL: "720h"}, sessionOverrides{ttl: 30 * 24 * time.Hour}},
		{"ttl surrounding space", types.HLSSessionOptions{TTL: " 90 "}, sessionOverrides{ttl: 90 * time.Second}},
		{"maxWidth even", types.HLSSessionOptions{MaxWidth: "1280"}, sessionOverrides{maxWidth: 1280}},
		{"maxWidth odd rounds down", types.HLSSessionOptions{MaxWidth: "1281"}, sessionOverrides{maxWidth: 1280}},
		{"maxHeight lower bound", types.HLSSessionOptions{MaxHeight: "16"}, sessionOverrides{maxHeight: 16}},
		{"maxHeight upper bound", types.HLSSessionOptions{MaxHeight: "7680"}, sessionOverrides{maxHeight: 7680}},
		{"bitrate M", types.HLSSessionOptions{Bitrate: "6M"}, sessionOverrides{videoBitrate: 6_000_000}},
		{"bitrate k lower bound", types.HLSSessionOptions{Bitrate: "100k"}, sessionOverrides{videoBitrate: 100_000}},
		{"bitrate plain upper bound", types.HLSSessionOptions{Bitrate: "200000000"}, sessionOverrides{videoBitrate: 200_000_000}},
		{"bitrate decimal M", types.HLSSessionOptions{Bitrate: "1.5M"}, sessionOverrides{videoBitrate: 1_500_000}},
		{"bitrate decimal below one M", types.HLSSessionOptions{Bitrate: "0.5M"}, sessionOverrides{videoBitrate: 500_000}},
		{"bitrate decimal k", types.HLSSessionOptions{Bitrate: "2500.5k"}, sessionOverrides{videoBitrate: 2_500_500}},
		{"bitrate decimal G upper bound", types.HLSSessionOptions{Bitrate: "0.2G"}, sessionOverrides{videoBitrate: 200_000_000}},
		{"bitrate equal to maxRate", types.HLSSessionOptions{Bitrate: "6M", MaxRate: "6000k"}, sessionOverrides{videoBitrate: 6_000_000, videoMaxrate: 6_000_000}},
		{"maxRate above bitrate", types.HLSSessionOptions{Bitrate: "1.5M", MaxRate: "2M"}, sessionOverrides{videoBitrate: 1_500_000, videoMaxrate: 2_000_000}},
		{"bufSize below bitrate is allowed", types.HLSSessionOptions{Bitrate: "6M", BufSize: "1M"}, sessionOverrides{videoBitrate: 6_000_000, videoBufsize: 1_000_000}},
		{"maxrate", types.HLSSessionOptions{MaxRate: "8000k"}, sessionOverrides{videoMaxrate: 8_000_000}},
		{"bufsize allows 2x bitrate cap", types.HLSSessionOptions{BufSize: "400M"}, sessionOverrides{videoBufsize: 400_000_000}},
		{
			"everything",
			types.HLSSessionOptions{TTL: "10m", MaxWidth: "1920", MaxHeight: "1080", Bitrate: "5M", MaxRate: "6M", BufSize: "12M"},
			sessionOverrides{ttl: 10 * time.Minute, maxWidth: 1920, maxHeight: 1080, videoBitrate: 5_000_000, videoMaxrate: 6_000_000, videoBufsize: 12_000_000},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseSessionOverrides(c.opts)
			if err != nil {
				t.Fatalf("parseSessionOverrides(%+v) error: %v", c.opts, err)
			}
			if got != c.want {
				t.Errorf("parseSessionOverrides(%+v) = %+v, want %+v", c.opts, got, c.want)
			}
		})
	}
}

func TestParseSessionOverridesInvalid(t *testing.T) {
	cases := []struct {
		name  string
		opts  types.HLSSessionOptions
		param string // must appear in the error message
	}{
		{"ttl zero", types.HLSSessionOptions{TTL: "0"}, "ttl"},
		{"ttl sub-second", types.HLSSessionOptions{TTL: "500ms"}, "ttl"},
		{"ttl one second", types.HLSSessionOptions{TTL: "1"}, "ttl"},
		{"ttl just below the 60s floor", types.HLSSessionOptions{TTL: "59s"}, "ttl"},
		{"ttl zero duration", types.HLSSessionOptions{TTL: "0s"}, "ttl"},
		{"ttl negative seconds", types.HLSSessionOptions{TTL: "-5"}, "ttl"},
		{"ttl negative duration", types.HLSSessionOptions{TTL: "-1m"}, "ttl"},
		{"ttl over 30 days", types.HLSSessionOptions{TTL: "721h"}, "ttl"},
		{"ttl huge seconds", types.HLSSessionOptions{TTL: "99999999999999999"}, "ttl"},
		{"ttl garbage", types.HLSSessionOptions{TTL: "soon"}, "ttl"},
		{"ttl days unit unsupported", types.HLSSessionOptions{TTL: "2d"}, "ttl"},
		{"maxWidth below min", types.HLSSessionOptions{MaxWidth: "15"}, "maxWidth"},
		{"maxWidth above max", types.HLSSessionOptions{MaxWidth: "7681"}, "maxWidth"},
		{"maxWidth zero", types.HLSSessionOptions{MaxWidth: "0"}, "maxWidth"},
		{"maxWidth fractional", types.HLSSessionOptions{MaxWidth: "720.5"}, "maxWidth"},
		{"maxHeight garbage", types.HLSSessionOptions{MaxHeight: "tall"}, "maxHeight"},
		{"maxHeight negative", types.HLSSessionOptions{MaxHeight: "-480"}, "maxHeight"},
		{"bitrate below min", types.HLSSessionOptions{Bitrate: "99k"}, "bitrate"},
		{"bitrate above max", types.HLSSessionOptions{Bitrate: "201M"}, "bitrate"},
		{"bitrate G above max", types.HLSSessionOptions{Bitrate: "1G"}, "bitrate"},
		{"bitrate decimal below min", types.HLSSessionOptions{Bitrate: "0.05M"}, "bitrate"},
		{"bitrate malformed decimal", types.HLSSessionOptions{Bitrate: "1.M"}, "bitrate"},
		{"bitrate leading dot", types.HLSSessionOptions{Bitrate: ".5M"}, "bitrate"},
		{"bitrate two dots", types.HLSSessionOptions{Bitrate: "1.5.2M"}, "bitrate"},
		{"bitrate unit suffix", types.HLSSessionOptions{Bitrate: "6Mbps"}, "bitrate"},
		{"bitrate garbage", types.HLSSessionOptions{Bitrate: "fast"}, "bitrate"},
		{"bitrate overflow does not wrap", types.HLSSessionOptions{Bitrate: "9223372036854775807k"}, "bitrate"},
		{"maxRate below min", types.HLSSessionOptions{MaxRate: "50k"}, "maxRate"},
		{"bufSize above max", types.HLSSessionOptions{BufSize: "401M"}, "bufSize"},
		{"maxRate below bitrate", types.HLSSessionOptions{Bitrate: "6M", MaxRate: "5M"}, "maxRate"},
		{"maxRate below decimal bitrate", types.HLSSessionOptions{Bitrate: "1.5M", MaxRate: "1400k"}, "maxRate"},
		{"one bad value fails the lot", types.HLSSessionOptions{TTL: "60", Bitrate: "6M", MaxHeight: "99999"}, "maxHeight"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseSessionOverrides(c.opts)
			if err == nil {
				t.Fatalf("parseSessionOverrides(%+v) accepted an invalid value", c.opts)
			}
			if !errors.Is(err, types.ErrInvalidHLSOption) {
				t.Errorf("error %v does not wrap types.ErrInvalidHLSOption", err)
			}
			if !strings.Contains(err.Error(), c.param) {
				t.Errorf("error %q does not name the offending parameter %q", err, c.param)
			}
		})
	}
}

// parseDecimalBitrate mirrors internal/app's envBitrate syntax (its
// ffmpegBitrateRe), so every value the env accepts for the rate knobs is
// also accepted as an override, and vice versa.
func TestParseDecimalBitrate(t *testing.T) {
	valid := map[string]int64{
		"8M": 8_000_000, "800k": 800_000, "8000000": 8_000_000, "1.5M": 1_500_000,
		"0.5m": 500_000, "2.25K": 2_250, "1G": 1_000_000_000, "1.5": 2, " 6M ": 6_000_000,
	}
	for in, want := range valid {
		if got, ok := parseDecimalBitrate(in); !ok || got != want {
			t.Errorf("parseDecimalBitrate(%q) = %d, %v; want %d, true", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "M", "1.M", ".5M", "1.5.2M", "-1M", "1e6", "6Mbps", "1,5M", "9223372036854775807k"} {
		if got, ok := parseDecimalBitrate(in); ok {
			t.Errorf("parseDecimalBitrate(%q) = %d, true; want rejected", in, got)
		}
	}
}

func TestParseFFmpegBitrateRejectsOverflow(t *testing.T) {
	for _, s := range []string{"9223372036854775807k", "9300000000000M", "9300000000G"} {
		if n, ok := parseFFmpegBitrate(s); ok {
			t.Errorf("parseFFmpegBitrate(%q) = %d, true; want overflow rejected", s, n)
		}
	}
	if n, ok := parseFFmpegBitrate("9223372036G"); !ok || n != 9_223_372_036_000_000_000 {
		t.Errorf("parseFFmpegBitrate(largest non-overflowing G) = %d, %v", n, ok)
	}
}

// ── effectiveSessionConfig precedence: override > /settings > env ─────────

func TestEffectiveSessionConfigSessionOverridePrecedence(t *testing.T) {
	env := DefaultHLSConfig()
	env.VideoBitrate, env.VideoMaxrate, env.VideoBufsize = "4M", "5M", "10M"
	env.MaxWidth, env.MaxHeight = 1920, 1080
	settings := stubSettings{
		"transcodeMaxBitRate": float64(3_000_000),
		"transcodeMaxWidth":   float64(1280),
	}

	cases := []struct {
		name     string
		ss       SettingsSource
		ov       sessionOverrides
		bitrate  string
		maxrate  string
		bufsize  string
		bps      int64
		maxW     int
		maxH     int
		override bool
	}{
		{
			name: "env only", ss: nil, ov: sessionOverrides{},
			bitrate: "4M", maxrate: "5M", bufsize: "10M", bps: 5_000_000, maxW: 1920, maxH: 1080,
		},
		{
			name: "settings beat env", ss: settings, ov: sessionOverrides{},
			bitrate: "3000000", maxrate: "3000000", bufsize: "6000000", bps: 3_000_000, maxW: 1280, maxH: 1080,
		},
		{
			name: "bitrate override beats settings and env, derives maxrate and 2x bufsize", ss: settings,
			ov:      sessionOverrides{videoBitrate: 6_000_000},
			bitrate: "6000000", maxrate: "6000000", bufsize: "12000000", bps: 6_000_000, maxW: 1280, maxH: 1080, override: true,
		},
		{
			name: "bitrate override beats env with no settings", ss: nil,
			ov:      sessionOverrides{videoBitrate: 1_000_000},
			bitrate: "1000000", maxrate: "1000000", bufsize: "2000000", bps: 1_000_000, maxW: 1920, maxH: 1080, override: true,
		},
		{
			name: "explicit maxrate and bufsize win over the bitrate-derived ones", ss: settings,
			ov:      sessionOverrides{videoBitrate: 6_000_000, videoMaxrate: 9_000_000, videoBufsize: 7_000_000},
			bitrate: "6000000", maxrate: "9000000", bufsize: "7000000", bps: 9_000_000, maxW: 1280, maxH: 1080, override: true,
		},
		{
			name: "lone maxrate leaves the settings bitrate and bufsize alone", ss: settings,
			ov:      sessionOverrides{videoMaxrate: 4_500_000},
			bitrate: "3000000", maxrate: "4500000", bufsize: "6000000", bps: 4_500_000, maxW: 1280, maxH: 1080, override: true,
		},
		{
			name: "lone bufsize leaves the env bitrate and maxrate alone", ss: nil,
			ov:      sessionOverrides{videoBufsize: 20_000_000},
			bitrate: "4M", maxrate: "5M", bufsize: "20000000", bps: 5_000_000, maxW: 1920, maxH: 1080, override: true,
		},
		{
			name: "maxWidth override beats transcodeMaxWidth, height keeps env", ss: settings,
			ov:      sessionOverrides{maxWidth: 854},
			bitrate: "3000000", maxrate: "3000000", bufsize: "6000000", bps: 3_000_000, maxW: 854, maxH: 1080, override: true,
		},
		{
			name: "maxHeight override beats env, width keeps settings", ss: settings,
			ov:      sessionOverrides{maxHeight: 480},
			bitrate: "3000000", maxrate: "3000000", bufsize: "6000000", bps: 3_000_000, maxW: 1280, maxH: 480, override: true,
		},
		{
			name: "ttl alone is not a quality override", ss: settings,
			ov:      sessionOverrides{ttl: time.Hour},
			bitrate: "3000000", maxrate: "3000000", bufsize: "6000000", bps: 3_000_000, maxW: 1280, maxH: 1080,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &hlsManager{cfg: env, settings: c.ss}
			sc := m.effectiveSessionConfig(c.ov)
			if sc.videoBitrate != c.bitrate || sc.videoMaxrate != c.maxrate || sc.videoBufsize != c.bufsize {
				t.Errorf("-b:v/-maxrate/-bufsize = %s/%s/%s, want %s/%s/%s",
					sc.videoBitrate, sc.videoMaxrate, sc.videoBufsize, c.bitrate, c.maxrate, c.bufsize)
			}
			if sc.maxrateBps != c.bps {
				t.Errorf("maxrateBps = %d, want %d", sc.maxrateBps, c.bps)
			}
			if sc.maxWidth != c.maxW || sc.maxHeight != c.maxH {
				t.Errorf("max dims = %dx%d, want %dx%d", sc.maxWidth, sc.maxHeight, c.maxW, c.maxH)
			}
			if sc.overridden != c.override {
				t.Errorf("overridden = %v, want %v", sc.overridden, c.override)
			}
			// Overrides never touch the non-overridable knobs.
			if sc.x264Preset != env.X264Preset || !sc.hwEnabled {
				t.Errorf("unrelated knobs changed: x264=%q hw=%v", sc.x264Preset, sc.hwEnabled)
			}
		})
	}
}

// With nothing overridden, the zero sessionOverrides must leave the config
// exactly as before this feature (byte-for-byte default compatibility).
func TestEffectiveSessionConfigZeroOverridesUnchanged(t *testing.T) {
	m := &hlsManager{cfg: DefaultHLSConfig()}
	sc := m.effectiveSessionConfig(sessionOverrides{})
	want := sessionConfig{
		videoBitrate: "8M", videoMaxrate: "8M", videoBufsize: "16M",
		maxrateBps: legacyMaxrateBps, hwEnabled: true, x264Preset: "veryfast", nvencPreset: "p4", qsvPreset: "veryfast",
	}
	if sc != want {
		t.Errorf("effectiveSessionConfig(zero overrides) = %+v, want %+v", sc, want)
	}
}

// ── BANDWIDTH/CODECS ───────────────────────────────────────────────────────

func TestDeriveBandwidthCodecsOverrideCountsAsConfigured(t *testing.T) {
	// Exactly the legacy 8M cap, no downscale — but requested explicitly per
	// session, so the derived path applies and CODECS follows the real 720p
	// output instead of the fixed legacy Level 4.1 string.
	sc := sessionConfig{maxrateBps: legacyMaxrateBps, overridden: true}
	bw, codecs := deriveBandwidthCodecs(sc, 1280, 720, false)
	if bw != 4_000_000 || codecs != "avc1.64001f" {
		t.Errorf("deriveBandwidthCodecs(overridden) = (%d,%q), want (4000000,\"avc1.64001f\")", bw, codecs)
	}
	// Same config without the override flag keeps the legacy strings.
	sc.overridden = false
	bw, codecs = deriveBandwidthCodecs(sc, 1280, 720, false)
	if bw != legacyBandwidth || codecs != legacyCodecsVideo {
		t.Errorf("deriveBandwidthCodecs(not overridden) = (%d,%q), want legacy", bw, codecs)
	}
}

// ── StartHLS end to end (probe cache seeded, no ffprobe) ───────────────────

const legacyStreamInf = `#EXT-X-STREAM-INF:BANDWIDTH=4000000,CODECS="avc1.640029,mp4a.40.2"`

func TestStartHLSGateOffIgnoresOverrides(t *testing.T) {
	m := newOverrideTestManager(t, false, DefaultHLSConfig(), nil)
	opts := types.HLSSessionOptions{TTL: "5", MaxHeight: "480", Bitrate: "1M", MaxRate: "not-a-rate", BufSize: "-1"}
	master, err := m.StartHLS("off", overrideMediaURL, opts)
	if err != nil {
		t.Fatalf("gate off: StartHLS must ignore (even invalid) overrides, got %v", err)
	}
	if !strings.Contains(master, legacyStreamInf) {
		t.Errorf("gate off: master changed from the legacy output:\n%s", master)
	}
	s := session(t, m, "off")
	if s.tc != m.effectiveSessionConfig(sessionOverrides{}) {
		t.Errorf("gate off: session config %+v differs from the no-override config", s.tc)
	}
	if got := s.ttl.Load(); got != 0 {
		t.Errorf("gate off: per-session ttl = %d, want 0 (global)", got)
	}
}

// Gate on but no parameters: output must be byte-identical to gate off.
func TestStartHLSGateOnWithoutParamsMatchesLegacy(t *testing.T) {
	off := newOverrideTestManager(t, false, DefaultHLSConfig(), nil)
	on := newOverrideTestManager(t, true, DefaultHLSConfig(), nil)
	a, errA := off.StartHLS("x", overrideMediaURL, types.HLSSessionOptions{})
	b, errB := on.StartHLS("x", overrideMediaURL, types.HLSSessionOptions{})
	if errA != nil || errB != nil {
		t.Fatalf("StartHLS errors: %v / %v", errA, errB)
	}
	if a != b {
		t.Errorf("gate on without params changed the master playlist:\noff:\n%s\non:\n%s", a, b)
	}
	if session(t, off, "x").tc != session(t, on, "x").tc {
		t.Error("gate on without params changed the session config")
	}
}

func TestStartHLSAppliesOverridesOnCreation(t *testing.T) {
	m := newOverrideTestManager(t, true, DefaultHLSConfig(), nil)
	master, err := m.StartHLS("party", overrideMediaURL, types.HLSSessionOptions{
		TTL: "3h", MaxHeight: "480", Bitrate: "1M",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := session(t, m, "party")
	if s.tc.videoBitrate != "1000000" || s.tc.videoMaxrate != "1000000" || s.tc.videoBufsize != "2000000" {
		t.Errorf("rate control = %s/%s/%s, want 1000000/1000000/2000000", s.tc.videoBitrate, s.tc.videoMaxrate, s.tc.videoBufsize)
	}
	if s.tc.maxHeight != 480 || s.tc.maxWidth != 0 {
		t.Errorf("max dims = %dx%d, want 0x480", s.tc.maxWidth, s.tc.maxHeight)
	}
	if got := time.Duration(s.ttl.Load()); got != 3*time.Hour {
		t.Errorf("per-session ttl = %s, want 3h", got)
	}

	// What transcodeSegment will hand ffmpeg for this 1920x1080 source.
	w, h, scaled := computeScaledDims(1920, 1080, s.tc.maxWidth, s.tc.maxHeight)
	if !scaled || w != 852 || h != 480 {
		t.Fatalf("computeScaledDims = %dx%d scaled=%v, want 852x480 scaled", w, h, scaled)
	}
	if vf := buildVideoFilter("libx264", true, w, h, scaled, tonemapPlan{}); vf != "format=yuv420p,scale=852:480" {
		t.Errorf("libx264 -vf = %q", vf)
	}

	// BANDWIDTH = maxrate/2, CODECS = Level 3.1 for 852x480.
	want := `#EXT-X-STREAM-INF:BANDWIDTH=500000,CODECS="avc1.64001f,mp4a.40.2"`
	if !strings.Contains(master, want) {
		t.Errorf("master missing %q:\n%s", want, master)
	}
}

func TestStartHLSOverrideBeatsSettingsInMaster(t *testing.T) {
	ss := stubSettings{"transcodeMaxBitRate": float64(3_000_000), "transcodeMaxWidth": float64(1280)}
	m := newOverrideTestManager(t, true, DefaultHLSConfig(), ss)

	// No override: /settings drive it (1280 wide → 1280x720, 3M → 1.5M).
	master, err := m.StartHLS("settings", overrideMediaURL, types.HLSSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := `BANDWIDTH=1500000,CODECS="avc1.64001f,`; !strings.Contains(master, want) {
		t.Errorf("settings-only master missing %q:\n%s", want, master)
	}

	// Override: bitrate and width both beat /settings.
	master, err = m.StartHLS("override", overrideMediaURL, types.HLSSessionOptions{Bitrate: "12M", MaxWidth: "2560"})
	if err != nil {
		t.Fatal(err)
	}
	// 2560 cap on a 1920 source never upscales → 1920x1080, Level 4.1; 12M → 6M.
	if want := `BANDWIDTH=6000000,CODECS="avc1.640029,`; !strings.Contains(master, want) {
		t.Errorf("override master missing %q:\n%s", want, master)
	}
}

func TestStartHLSInvalidOverrideRejectedBeforeSessionCreated(t *testing.T) {
	m := newOverrideTestManager(t, true, DefaultHLSConfig(), nil)
	_, err := m.StartHLS("bad", overrideMediaURL, types.HLSSessionOptions{Bitrate: "300M"})
	if !errors.Is(err, types.ErrInvalidHLSOption) {
		t.Fatalf("StartHLS error = %v, want one wrapping types.ErrInvalidHLSOption", err)
	}
	if n := m.Sessions(); n != 0 {
		t.Errorf("Sessions() = %d after a rejected override, want 0", n)
	}
	if _, statErr := os.Stat(filepath.Join(m.base, "bad")); !os.IsNotExist(statErr) {
		t.Error("session dir created for a rejected override")
	}
}

// Overrides must not weaken the SSRF gate or the session-id guard: both are
// checked first and fail with their own (non-400) errors.
func TestStartHLSOverridesDoNotBypassSecurityChecks(t *testing.T) {
	m := newOverrideTestManager(t, true, DefaultHLSConfig(), nil)
	opts := types.HLSSessionOptions{TTL: "720h", Bitrate: "200M", MaxWidth: "7680"}
	for _, c := range []struct{ id, url string }{
		{"ok", "http://127.0.0.1:1/x.mkv"},
		{"ok", "http://169.254.169.254/latest/meta-data"},
		{"ok", "file:///etc/passwd"},
		{"../escape", overrideMediaURL},
	} {
		_, err := m.StartHLS(c.id, c.url, opts)
		if err == nil {
			t.Errorf("StartHLS(%q, %q) with overrides accepted", c.id, c.url)
			continue
		}
		if errors.Is(err, types.ErrInvalidHLSOption) {
			t.Errorf("StartHLS(%q, %q) failed on the overrides instead of the security check: %v", c.id, c.url, err)
		}
	}
	if n := m.Sessions(); n != 0 {
		t.Errorf("Sessions() = %d, want 0", n)
	}
}

func TestStartHLSExistingSessionOnlyTTLUpdates(t *testing.T) {
	m := newOverrideTestManager(t, true, DefaultHLSConfig(), nil)
	first, err := m.StartHLS("room", overrideMediaURL, types.HLSSessionOptions{TTL: "10m", Bitrate: "2M"})
	if err != nil {
		t.Fatal(err)
	}
	s := session(t, m, "room")
	tc := s.tc

	// Later quality params are ignored: tc and the master are unchanged.
	again, err := m.StartHLS("room", overrideMediaURL, types.HLSSessionOptions{Bitrate: "6M", MaxHeight: "480"})
	if err != nil {
		t.Fatal(err)
	}
	if s.tc != tc || again != first {
		t.Errorf("quality overrides on an existing session changed it:\ntc %+v -> %+v\n%s\n%s", tc, s.tc, first, again)
	}
	if got := time.Duration(s.ttl.Load()); got != 10*time.Minute {
		t.Errorf("request without ttl changed the session ttl to %s, want 10m kept", got)
	}

	// ttl on a later request replaces the session's TTL (extend...)
	if _, err := m.StartHLS("room", overrideMediaURL, types.HLSSessionOptions{TTL: "6h"}); err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(s.ttl.Load()); got != 6*time.Hour {
		t.Errorf("ttl after extend = %s, want 6h", got)
	}
	// ... or shorten.
	if _, err := m.StartHLS("room", overrideMediaURL, types.HLSSessionOptions{TTL: "90"}); err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(s.ttl.Load()); got != 90*time.Second {
		t.Errorf("ttl after shorten = %s, want 90s", got)
	}

	// Invalid values are still rejected for an existing session, and leave it
	// untouched: neither above the 30-day cap nor below the 60s floor (a live
	// session must never be shrunk below stock durability).
	for _, bad := range []string{"1000h", "1", "59s", "500ms"} {
		if _, err := m.StartHLS("room", overrideMediaURL, types.HLSSessionOptions{TTL: bad}); !errors.Is(err, types.ErrInvalidHLSOption) {
			t.Errorf("ttl=%s on existing session: err = %v, want ErrInvalidHLSOption", bad, err)
		}
		if got := time.Duration(s.ttl.Load()); got != 90*time.Second {
			t.Errorf("rejected ttl=%s changed the session ttl to %s", bad, got)
		}
	}

	// A session created without ttl can gain one later.
	if _, err := m.StartHLS("plain", overrideMediaURL, types.HLSSessionOptions{}); err != nil {
		t.Fatal(err)
	}
	p := session(t, m, "plain")
	if p.ttl.Load() != 0 {
		t.Fatal("session created without ttl has a per-session ttl")
	}
	if _, err := m.StartHLS("plain", overrideMediaURL, types.HLSSessionOptions{TTL: "2h"}); err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(p.ttl.Load()); got != 2*time.Hour {
		t.Errorf("ttl added to existing session = %s, want 2h", got)
	}
}

// ── per-session TTL eviction ───────────────────────────────────────────────

func TestEvictIdleHonoursPerSessionTTL(t *testing.T) {
	m := newTestHLSManager(t)
	// A global TTL above the 60s override floor, so a valid per-session TTL
	// can be both shorter and longer than it.
	m.cfg.SessionTTL = 10 * time.Minute
	global := m.cfg.SessionTTL
	now := time.Now()

	cases := []struct {
		id        string
		ttl       time.Duration // 0 = global
		idleFor   time.Duration
		wantEvict bool
	}{
		{"short-expired", 90 * time.Second, 2 * time.Minute, true},
		{"short-fresh", 90 * time.Second, 30 * time.Second, false},
		{"long-past-global", time.Hour, 2 * global, false},
		{"long-expired", time.Hour, 2 * time.Hour, true},
		{"global-expired", 0, 2 * global, true},
		{"global-fresh", 0, global / 2, false},
	}
	m.mu.Lock()
	for _, c := range cases {
		dir := filepath.Join(m.base, c.id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			m.mu.Unlock()
			t.Fatal(err)
		}
		s := &hlsSession{dir: dir, segLocks: map[string]*sync.Mutex{}}
		s.ttl.Store(int64(c.ttl))
		s.lastAccess.Store(now.Add(-c.idleFor).UnixNano())
		m.sessions[c.id] = s
	}
	m.mu.Unlock()

	m.evictIdle()

	for _, c := range cases {
		m.mu.Lock()
		_, present := m.sessions[c.id]
		m.mu.Unlock()
		_, statErr := os.Stat(filepath.Join(m.base, c.id))
		dirGone := os.IsNotExist(statErr)
		if c.wantEvict && (present || !dirGone) {
			t.Errorf("%s: want evicted (ttl %s, idle %s), present=%v dirGone=%v", c.id, c.ttl, c.idleFor, present, dirGone)
		}
		if !c.wantEvict && (!present || dirGone) {
			t.Errorf("%s: want kept (ttl %s, idle %s), present=%v dirGone=%v", c.id, c.ttl, c.idleFor, present, dirGone)
		}
	}
}

// TestEvictIdleDisableIdleEvictionHonoursPerSessionTTL pins the
// DisableIdleEviction (STREMIO_HLS_SESSION_TTL=0) x per-session ?ttl=
// interaction: an explicit per-session ttl still wins and gets evicted on
// schedule even while global idle eviction is off, while a session with no
// ttl of its own is never evicted in that mode, however stale.
func TestEvictIdleDisableIdleEvictionHonoursPerSessionTTL(t *testing.T) {
	m := newTestHLSManager(t)
	m.cfg.DisableIdleEviction = true
	m.cfg.SessionTTL = 10 * time.Minute // must be ignored entirely
	now := time.Now()

	cases := []struct {
		id        string
		ttl       time.Duration // 0 = no per-session ttl (relies on global)
		idleFor   time.Duration
		wantEvict bool
	}{
		{"per-session-expired", 90 * time.Second, time.Hour, true},
		{"per-session-fresh", 90 * time.Second, 10 * time.Second, false},
		{"global-only-ancient", 0, 365 * 24 * time.Hour, false}, // never evicted
	}
	m.mu.Lock()
	for _, c := range cases {
		dir := filepath.Join(m.base, c.id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			m.mu.Unlock()
			t.Fatal(err)
		}
		s := &hlsSession{dir: dir, segLocks: map[string]*sync.Mutex{}}
		s.ttl.Store(int64(c.ttl))
		s.lastAccess.Store(now.Add(-c.idleFor).UnixNano())
		m.sessions[c.id] = s
	}
	m.mu.Unlock()

	m.evictIdle()

	for _, c := range cases {
		m.mu.Lock()
		_, present := m.sessions[c.id]
		m.mu.Unlock()
		_, statErr := os.Stat(filepath.Join(m.base, c.id))
		dirGone := os.IsNotExist(statErr)
		if c.wantEvict && (present || !dirGone) {
			t.Errorf("%s: want evicted (ttl %s, idle %s), present=%v dirGone=%v", c.id, c.ttl, c.idleFor, present, dirGone)
		}
		if !c.wantEvict && (!present || dirGone) {
			t.Errorf("%s: want kept (ttl %s, idle %s), present=%v dirGone=%v", c.id, c.ttl, c.idleFor, present, dirGone)
		}
	}
}

// The reaper keeps ticking even with DisableIdleEviction set, so a
// per-session ttl is reaped promptly rather than only at shutdown.
func TestReaperReapsPerSessionTTLWithDisableIdleEviction(t *testing.T) {
	m := newTestHLSManager(t)
	m.cfg.DisableIdleEviction = true
	m.cfg.SessionTTL = time.Hour // irrelevant: global eviction is off
	m.cfg.ReaperInterval = 20 * time.Millisecond
	dir := filepath.Join(m.base, "short")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := &hlsSession{dir: dir, segLocks: map[string]*sync.Mutex{}}
	s.ttl.Store(int64(50 * time.Millisecond))
	s.lastAccess.Store(time.Now().UnixNano())
	m.mu.Lock()
	m.sessions["short"] = s
	m.mu.Unlock()

	go m.reaper()
	defer m.CloseHLS()

	deadline := time.Now().Add(5 * time.Second)
	for m.Sessions() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("reaper did not evict a per-session-ttl session while DisableIdleEviction is set")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A per-session TTL shorter than the global one is reaped by the running
// reaper within one ReaperInterval of expiring (the reaper interval is not
// derived from per-session TTLs; see evictIdle's doc comment). The TTL is
// stored directly, bypassing the 60s validation floor, purely so the test
// runs in milliseconds; it exercises the reaper mechanism, not the parser.
func TestReaperReapsShortPerSessionTTL(t *testing.T) {
	m := newTestHLSManager(t)
	m.cfg.SessionTTL = time.Hour // global far away
	m.cfg.ReaperInterval = 20 * time.Millisecond
	dir := filepath.Join(m.base, "short")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := &hlsSession{dir: dir, segLocks: map[string]*sync.Mutex{}}
	s.ttl.Store(int64(50 * time.Millisecond))
	s.lastAccess.Store(time.Now().UnixNano())
	m.mu.Lock()
	m.sessions["short"] = s
	m.mu.Unlock()

	go m.reaper()
	defer m.CloseHLS()

	deadline := time.Now().Add(5 * time.Second)
	for m.Sessions() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("reaper did not evict a session whose per-session TTL (50ms) is far below the global TTL (1h)")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
