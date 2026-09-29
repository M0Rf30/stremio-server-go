package media

import (
	"strings"
	"testing"
	"time"
)

// ── Container-aware seek pre-roll (STREMIO_HLS_SEEK_PREROLL) ────────────────

func TestIsSeekIndexed(t *testing.T) {
	cases := []struct {
		format string
		want   bool
	}{
		{"matroska,webm", true},
		{"mov,mp4,m4a,3gp,3g2,mj2", true},
		{"mpegts", false}, // .ts and .m2ts: no seek index
		{"hls", false},    // an .m3u8 mediaURL: unindexed MPEG-TS underneath
		{"flv", false},
		{"avi", false}, // not measured: full margin until it is
		{"h264", false},
		{"hevc", false},
		{"mp3", false},
		{"ipmovie", false},  // contains "mov" but has no index
		{"wc3movie", false}, // likewise
		{"webm", true},
		{"mp4", true},
		{"", false}, // unknown
	}
	for _, c := range cases {
		if got := isSeekIndexed(c.format); got != c.want {
			t.Errorf("isSeekIndexed(%q) = %v, want %v", c.format, got, c.want)
		}
	}
}

func TestHLSConfigNormalizeSeekPreroll(t *testing.T) {
	cases := []struct {
		in, want time.Duration
	}{
		{0, 10 * time.Second}, // unset: default
		{2 * time.Second, 2 * time.Second},
		{2500 * time.Millisecond, 2500 * time.Millisecond},
		{SeekPrerollNone, SeekPrerollNone},
		{-5 * time.Second, SeekPrerollNone},                                     // any negative canonicalizes
		{10 * time.Second, 10 * time.Second},                                    // at the cap
		{30 * time.Second, MaxSeekPreroll},                                      // only ever shrinks
		{5 * time.Minute, MaxSeekPreroll},                                       // clamped
		{time.Millisecond, time.Millisecond},                                    // tiny but positive: honoured
		{MaxSeekPreroll + 1, MaxSeekPreroll},                                    // just above the cap
		{MaxSeekPreroll - time.Millisecond, MaxSeekPreroll - time.Millisecond},  // just below
		{2500*time.Millisecond + 400*time.Microsecond, 2500 * time.Millisecond}, // truncated to ms
		{500 * time.Microsecond, SeekPrerollNone},                               // below 1ms: none
	}
	for _, c := range cases {
		cfg := HLSConfig{SeekPreroll: c.in}
		if got := cfg.normalize(1).SeekPreroll; got != c.want {
			t.Errorf("normalize(SeekPreroll=%s) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestSegPreRollPlan(t *testing.T) {
	cases := []struct {
		name        string
		preroll     time.Duration
		isTS        bool
		seekIndexed bool
		want        preRollPlan
	}{
		// Defaults: exactly today's behaviour for every container.
		{"default indexed", 0, false, true, preRollPlan{first: 10}},
		{"default TS", 0, true, false, preRollPlan{first: 10, retry: 30}},
		{"default other", 0, false, false, preRollPlan{first: 10}},
		// A shrunk pre-roll applies to indexed containers only, with the
		// full margin as the retry.
		{"2s indexed", 2 * time.Second, false, true, preRollPlan{first: 2, retry: 10}},
		{"none indexed", SeekPrerollNone, false, true, preRollPlan{first: 0, retry: 10}},
		{"2.5s indexed", 2500 * time.Millisecond, false, true, preRollPlan{first: 2.5, retry: 10}},
		{"2s TS", 2 * time.Second, true, false, preRollPlan{first: 10, retry: 30}},
		{"none TS", SeekPrerollNone, true, false, preRollPlan{first: 10, retry: 30}},
		{"2s other", 2 * time.Second, false, false, preRollPlan{first: 10}},
		{"none other", SeekPrerollNone, false, false, preRollPlan{first: 10}},
		{"10s indexed is the default", 10 * time.Second, false, true, preRollPlan{first: 10}},
		{"above the cap indexed", time.Minute, false, true, preRollPlan{first: 10}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &hlsManager{cfg: HLSConfig{SeekPreroll: c.preroll}.normalize(1)}
			if got := m.segPreRollPlan(c.isTS, c.seekIndexed); got != c.want {
				t.Errorf("segPreRollPlan(isTS=%v, indexed=%v) = %+v, want %+v", c.isTS, c.seekIndexed, got, c.want)
			}
		})
	}
}

func TestHybridSeek(t *testing.T) {
	cases := []struct {
		start, preRoll, wantInput, wantOutput float64
	}{
		{0, 10, 0, 0},
		{4, 10, 0, 4}, // near the start: the input seek floors at 0
		{28, 10, 18, 10},
		{80, 10, 70, 10},
		{80, 30, 50, 30},
		{28, 2, 26, 2},
		{28, 2.5, 25.5, 2.5},
		{28, 0, 28, 0}, // pure input seek
		{0, 0, 0, 0},
		{28, -1, 28, 0}, // defensive: a negative margin is none
	}
	for _, c := range cases {
		in, out := hybridSeek(c.start, c.preRoll)
		if in != c.wantInput || out != c.wantOutput {
			t.Errorf("hybridSeek(%v, %v) = (%v, %v), want (%v, %v)",
				c.start, c.preRoll, in, out, c.wantInput, c.wantOutput)
		}
	}
}

// testSegmentJob builds the segmentJob transcodeSegment would for segment n
// of a long 8-bit SDR source with no downscale.
func testSegmentJob(m *hlsManager, n int, kind segKind) segmentJob {
	return segmentJob{
		kind:     kind,
		inputURL: "http://127.0.0.1:11470/src",
		protoWL:  "http,tcp",
		start:    float64(n) * segDur,
		dur:      segDur,
		tc:       m.effectiveSessionConfig(sessionOverrides{}),
	}
}

// TestBuildSegmentArgsGoldenDefaults pins the exact libx264 argv an
// unconfigured server has always produced for a mid-file muxed segment
// (segment 7, start 28s: -ss 18 before -i, -ss 10 after it).
func TestBuildSegmentArgsGoldenDefaults(t *testing.T) {
	m := &hlsManager{cfg: DefaultHLSConfig().normalize(1)}
	pre := m.segPreRollPlan(false, true) // an MKV at the default
	got := strings.Join(m.buildSegmentArgs(testSegmentJob(m, 7, segMuxed), hwEncoder{codec: "libx264"}, pre.first, "seg.ts.tmp.ts"), " ")
	want := "-hide_banner -loglevel error -y -fflags +genpts -analyzeduration 2000000 -probesize 2000000 " +
		"-ss 18.000 -protocol_whitelist http,tcp -i http://127.0.0.1:11470/src -ss 10.000 " +
		"-map 0:v:0? -map 0:a:0? " +
		"-vf format=yuv420p -c:v libx264 -preset veryfast -crf 23 -profile:v high -sc_threshold 0 " +
		"-g 120 -keyint_min 120 -b:v 8M -maxrate 8M -bufsize 16M " +
		"-c:a aac -ac 2 -b:a 192k -af aresample=async=1:first_pts=0,apad -sn " +
		"-output_ts_offset 28.000 -muxdelay 0 -t 4.000 -mpegts_copyts 1 -f mpegts seg.ts.tmp.ts"
	if got != want {
		t.Errorf("default argv changed:\n got %s\nwant %s", got, want)
	}
}

// seekArgs returns the -ss values placed before and after -i ("" if absent).
func seekArgs(args []string) (before, after string) {
	seenInput := false
	for i := 0; i < len(args)-1; i++ {
		switch args[i] {
		case "-i":
			seenInput = true
		case "-ss":
			if seenInput {
				after = args[i+1]
			} else {
				before = args[i+1]
			}
		}
	}
	return before, after
}

// TestBuildSegmentArgsSeekPreroll checks the -ss pair of the first attempt
// for each container class: the knob moves only the indexed container's
// seek, and nothing else in the argv changes.
func TestBuildSegmentArgsSeekPreroll(t *testing.T) {
	cases := []struct {
		name        string
		preroll     time.Duration
		isTS        bool
		seekIndexed bool
		n           int
		wantBefore  string
		wantAfter   string
	}{
		{"default MKV seg0", 0, false, true, 0, "", ""},
		{"default MKV seg1", 0, false, true, 1, "", "4.000"},
		{"default MKV seg20", 0, false, true, 20, "70.000", "10.000"},
		{"2s MKV seg7", 2 * time.Second, false, true, 7, "26.000", "2.000"},
		{"2.5s MKV seg20", 2500 * time.Millisecond, false, true, 20, "77.500", "2.500"},
		{"none MKV seg7", SeekPrerollNone, false, true, 7, "28.000", ""},
		{"none MKV seg0", SeekPrerollNone, false, true, 0, "", ""},
		{"2s TS seg20 keeps the full margin", 2 * time.Second, true, false, 20, "70.000", "10.000"},
		{"none TS seg7 keeps the full margin", SeekPrerollNone, true, false, 7, "18.000", "10.000"},
		{"2s unknown container seg20", 2 * time.Second, false, false, 20, "70.000", "10.000"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := DefaultHLSConfig()
			cfg.SeekPreroll = c.preroll
			m := &hlsManager{cfg: cfg.normalize(1)}
			def := &hlsManager{cfg: DefaultHLSConfig().normalize(1)}
			pre := m.segPreRollPlan(c.isTS, c.seekIndexed)
			for _, kind := range []segKind{segMuxed, segVideoOnly, segAudioOnly} {
				args := m.buildSegmentArgs(testSegmentJob(m, c.n, kind), hwEncoder{codec: "libx264"}, pre.first, "out.ts")
				before, after := seekArgs(args)
				if before != c.wantBefore || after != c.wantAfter {
					t.Errorf("kind %d: -ss before/after -i = %q/%q, want %q/%q (argv %v)",
						kind, before, after, c.wantBefore, c.wantAfter, args)
				}
				// Everything but the seek matches the default argv.
				base := def.buildSegmentArgs(testSegmentJob(def, c.n, kind), hwEncoder{codec: "libx264"}, fullPreRoll, "out.ts")
				if a, b := stripSeek(args), stripSeek(base); a != b {
					t.Errorf("kind %d: argv differs beyond -ss:\n got %s\nwant %s", kind, a, b)
				}
			}
		})
	}
}

// stripSeek joins args with every "-ss <value>" pair removed.
func stripSeek(args []string) string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-ss" {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return strings.Join(out, " ")
}

// TestBuildSegmentArgsTonemapTrimFollowsPreRoll: the tone-map chain's trim
// tracks each attempt's own output seek, so a shrunk pre-roll and its
// full-margin retry each trim exactly what their -ss discards.
func TestBuildSegmentArgsTonemapTrimFollowsPreRoll(t *testing.T) {
	m := &hlsManager{cfg: DefaultHLSConfig().normalize(1)}
	j := testSegmentJob(m, 20, segMuxed)
	j.tm = planTonemap("mobius", videoColor{transfer: "smpte2084", primaries: "bt2020"})
	if !j.tm.enabled() {
		t.Fatal("tone-map plan not enabled for a PQ source")
	}
	for _, preRoll := range []float64{0, 2, 10} {
		args := strings.Join(m.buildSegmentArgs(j, hwEncoder{codec: "libx264"}, preRoll, "out.ts"), " ")
		rtm := j.tm
		_, rtm.trimStart = hybridSeek(j.start, preRoll)
		want := videoEncodeArgs("libx264", false, 0, 0, false, j.tc, m.cfg, rtm)
		if !strings.Contains(args, strings.Join(want, " ")) {
			t.Errorf("pre-roll %v: video args not trimmed to the output seek:\n argv %s\nwant %v", preRoll, args, want)
		}
	}
}

// TestOutputFingerprintPreRoll: the fingerprint of every session at the
// default margin is unchanged, and a shrunk pre-roll changes it only for
// the indexed sessions it applies to.
func TestOutputFingerprintPreRoll(t *testing.T) {
	def := &hlsManager{cfg: DefaultHLSConfig().normalize(1)}
	tc := def.effectiveSessionConfig(sessionOverrides{})
	base := def.outputFingerprint(tc, def.segPreRollPlan(false, true))
	if strings.Contains(base, "preroll") {
		t.Errorf("default fingerprint mentions the pre-roll: %q", base)
	}
	for _, ts := range []bool{false, true} {
		if got := def.outputFingerprint(tc, def.segPreRollPlan(ts, false)); got != base {
			t.Errorf("default fingerprint differs by container (isTS=%v): %q vs %q", ts, got, base)
		}
	}
	cfg := DefaultHLSConfig()
	cfg.SeekPreroll = 2 * time.Second
	m := &hlsManager{cfg: cfg.normalize(1)}
	if got := m.outputFingerprint(tc, m.segPreRollPlan(false, true)); got != base+" preroll=2.000" {
		t.Errorf("indexed fingerprint with a 2s pre-roll = %q", got)
	}
	if got := m.outputFingerprint(tc, m.segPreRollPlan(true, false)); got != base {
		t.Errorf("TS fingerprint changed with the indexed pre-roll: %q", got)
	}
}
