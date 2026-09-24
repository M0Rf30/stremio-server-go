package app

// Tests for the issue #20 config-parsing helpers added to lookup.go/app.go:
// envDuration (duration string + plain seconds, invalid -> default),
// envBitrate, and the hlsConfig() env -> media.HLSConfig builder.

import (
	"testing"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/media"
)

func TestEnvDurationParsesDurationString(t *testing.T) {
	lookup := MapLookup(map[string]string{"K": "5m30s"})
	got := envDuration(lookup, "K", time.Second)
	want := 5*time.Minute + 30*time.Second
	if got != want {
		t.Errorf("envDuration(%q) = %s, want %s", "5m30s", got, want)
	}
}

func TestEnvDurationParsesPlainSeconds(t *testing.T) {
	lookup := MapLookup(map[string]string{"K": "90"})
	if got := envDuration(lookup, "K", time.Second); got != 90*time.Second {
		t.Errorf("envDuration(%q) = %s, want 90s", "90", got)
	}
}

func TestEnvDurationUnsetReturnsDefault(t *testing.T) {
	lookup := MapLookup(map[string]string{})
	def := 42 * time.Second
	if got := envDuration(lookup, "K", def); got != def {
		t.Errorf("envDuration(unset) = %s, want default %s", got, def)
	}
}

func TestEnvDurationInvalidReturnsDefault(t *testing.T) {
	def := 7 * time.Second
	cases := []string{"not-a-duration", "1.5.3s", "abc123", ""}
	for _, v := range cases {
		lookup := MapLookup(map[string]string{"K": v})
		if got := envDuration(lookup, "K", def); got != def {
			t.Errorf("envDuration(%q) = %s, want default %s", v, got, def)
		}
	}
}

func TestEnvDurationNegativeReturnsDefault(t *testing.T) {
	def := 7 * time.Second
	for _, v := range []string{"-5s", "-5"} {
		lookup := MapLookup(map[string]string{"K": v})
		if got := envDuration(lookup, "K", def); got != def {
			t.Errorf("envDuration(%q) = %s, want default %s (negative rejected)", v, got, def)
		}
	}
}

func TestEnvBitrateValidValues(t *testing.T) {
	for _, v := range []string{"8M", "800k", "8000000", "1.5M"} {
		lookup := MapLookup(map[string]string{"K": v})
		if got := envBitrate(lookup, "K", "8M"); got != v {
			t.Errorf("envBitrate(%q) = %q, want %q unchanged", v, got, v)
		}
	}
}

func TestEnvBitrateInvalidReturnsDefault(t *testing.T) {
	def := "8M"
	for _, v := range []string{"eight megabits", "8Mi", "-8M", "8 M"} {
		lookup := MapLookup(map[string]string{"K": v})
		if got := envBitrate(lookup, "K", def); got != def {
			t.Errorf("envBitrate(%q) = %q, want default %q", v, got, def)
		}
	}
}

func TestEnvBitrateUnsetReturnsDefault(t *testing.T) {
	lookup := MapLookup(map[string]string{})
	if got := envBitrate(lookup, "K", "16M"); got != "16M" {
		t.Errorf("envBitrate(unset) = %q, want default", got)
	}
}

// ── hlsConfig() ──────────────────────────────────────────────────────────────

func TestHLSConfigDefaultsWhenNothingSet(t *testing.T) {
	got := hlsConfig(MapLookup(map[string]string{}))
	want := media.DefaultHLSConfig()
	if got != want {
		t.Errorf("hlsConfig(empty env) = %+v, want DefaultHLSConfig() %+v", got, want)
	}
}

func TestHLSConfigSessionTTLOverrideDerivesReaperInterval(t *testing.T) {
	// STREMIO_HLS_REAPER_INTERVAL left unset: its default must scale with the
	// overridden SessionTTL (issue #20: "Should probably scale with the TTL
	// above"), not stay pinned at the historical 30s.
	got := hlsConfig(MapLookup(map[string]string{"STREMIO_HLS_SESSION_TTL": "10s"}))
	if got.SessionTTL != 10*time.Second {
		t.Fatalf("SessionTTL = %s, want 10s", got.SessionTTL)
	}
	want := media.DefaultReaperInterval(10 * time.Second)
	if got.ReaperInterval != want {
		t.Errorf("ReaperInterval = %s, want derived %s", got.ReaperInterval, want)
	}
}

func TestHLSConfigReaperIntervalExplicitOverride(t *testing.T) {
	got := hlsConfig(MapLookup(map[string]string{
		"STREMIO_HLS_SESSION_TTL":     "10s",
		"STREMIO_HLS_REAPER_INTERVAL": "3s",
	}))
	if got.ReaperInterval != 3*time.Second {
		t.Errorf("explicit STREMIO_HLS_REAPER_INTERVAL not honoured: got %s, want 3s", got.ReaperInterval)
	}
}

func TestHLSConfigEveryKnobOverridable(t *testing.T) {
	env := map[string]string{
		"STREMIO_HLS_SESSION_TTL":          "45s",
		"STREMIO_HLS_NEG_PROBE_TTL":        "2m",
		"STREMIO_HLS_POS_PROBE_TTL":        "3m",
		"STREMIO_HLS_MAX_SESSIONS":         "10",
		"STREMIO_HLS_WORK_DIR":             "/data/hls",
		"STREMIO_TRANSCODE_VIDEO_BITRATE":  "4M",
		"STREMIO_TRANSCODE_MAXRATE":        "5M",
		"STREMIO_TRANSCODE_BUFSIZE":        "10M",
		"STREMIO_TRANSCODE_MAX_WIDTH":      "1280",
		"STREMIO_TRANSCODE_MAX_HEIGHT":     "720",
		"STREMIO_TRANSCODE_VAAPI_QP":       "18",
		"STREMIO_TRANSCODE_NVENC_PRESET":   "p1",
		"STREMIO_TRANSCODE_X264_PRESET":    "fast",
		"STREMIO_TRANSCODE_X264_CRF":       "20",
		"STREMIO_TRANSCODE_AUDIO_CHANNELS": "6",
		"STREMIO_TRANSCODE_AUDIO_BITRATE":  "256k",
		"STREMIO_TRANSCODE_CONCURRENCY":    "2",
		"STREMIO_HLS_VAAPI_DEVICE":         "/dev/dri/renderD129",
		"STREMIO_HLS_SEGMENT_TIMEOUT":      "60s",
		"STREMIO_HLS_SUBTITLE_TIMEOUT":     "45s",
		"STREMIO_HLS_PROBE_TIMEOUT":        "15s",
	}
	got := hlsConfig(MapLookup(env))
	want := media.HLSConfig{
		SessionTTL:         45 * time.Second,
		ReaperInterval:     media.DefaultReaperInterval(45 * time.Second),
		NegProbeTTL:        2 * time.Minute,
		PosProbeTTL:        3 * time.Minute,
		MaxSessions:        10,
		WorkDir:            "/data/hls",
		VideoBitrate:       "4M",
		VideoMaxrate:       "5M",
		VideoBufsize:       "10M",
		MaxWidth:           1280,
		MaxHeight:          720,
		VAAPIQP:            18,
		NVENCPreset:        "p1",
		X264Preset:         "fast",
		X264CRF:            20,
		AudioChannels:      6,
		AudioBitrate:       "256k",
		SegmentConcurrency: 2,
		VAAPIDevice:        "/dev/dri/renderD129",
		SegmentTimeout:     60 * time.Second,
		SubtitleTimeout:    45 * time.Second,
		ProbeTimeout:       15 * time.Second,
	}
	if got != want {
		t.Errorf("hlsConfig(full env) = %+v,\nwant %+v", got, want)
	}
}

func TestHLSConfigNegativeMaxWidthHeightRejected(t *testing.T) {
	got := hlsConfig(MapLookup(map[string]string{
		"STREMIO_TRANSCODE_MAX_WIDTH":  "-1",
		"STREMIO_TRANSCODE_MAX_HEIGHT": "-1",
	}))
	if got.MaxWidth != 0 || got.MaxHeight != 0 {
		t.Errorf("negative MaxWidth/MaxHeight must fall back to default 0, got %d/%d", got.MaxWidth, got.MaxHeight)
	}
}

func TestHLSConfigCreateMetadataWaitViaTypesConfig(t *testing.T) {
	// CreateMetadataWait lives on types.Config (built in Run), not
	// media.HLSConfig; covered by envDuration directly plus the run_test.go
	// integration coverage. This is a smoke check that the same helper used
	// there behaves for the exact env name Run wires it to.
	lookup := MapLookup(map[string]string{"STREMIO_CREATE_METADATA_TIMEOUT": "45"})
	if got := envDuration(lookup, "STREMIO_CREATE_METADATA_TIMEOUT", 90*time.Second); got != 45*time.Second {
		t.Errorf("STREMIO_CREATE_METADATA_TIMEOUT = %s, want 45s", got)
	}
}
