// SPDX-FileCopyrightText: 2026 The rivulet-engine Authors
//
// SPDX-License-Identifier: MIT

package api

// Handler tests for the per-session HLS overrides on
// GET /hlsv2/{id}/master.m3u8: query pass-through to MediaProber.StartHLS
// and the 400 mapping for types.ErrInvalidHLSOption. Parsing, validation and
// the RIVULET_HLS_SESSION_OVERRIDES gate live in internal/media and are
// tested there (session_overrides_test.go).

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/rivulet-kodi/rivulet-engine/internal/types"
)

func TestHandlerHLS_MasterPassesOverridesThrough(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  types.HLSSessionOptions
	}{
		{"none", "mediaURL=http://example.com/a.mkv", types.HLSSessionOptions{}},
		{
			"all known params",
			"mediaURL=http://example.com/a.mkv&ttl=3h&maxWidth=1280&maxHeight=720&bitrate=6M&maxRate=7M&bufSize=12M",
			types.HLSSessionOptions{TTL: "3h", MaxWidth: "1280", MaxHeight: "720", Bitrate: "6M", MaxRate: "7M", BufSize: "12M"},
		},
		{
			"unknown params are ignored",
			"mediaURL=http://example.com/a.mkv&bitrate=6M&crf=1&preset=placebo&audioBitrate=1M&hwaccel=0",
			types.HLSSessionOptions{Bitrate: "6M"},
		},
		{
			// Names are case-sensitive camelCase (maxWidth, maxRate, bufSize);
			// any other casing is just an unknown parameter and is ignored.
			"mis-cased names are not applied",
			"mediaURL=http://example.com/a.mkv&maxrate=2M&bufsize=4M&MaxWidth=1280&maxheight=720&BITRATE=1M&TTL=3h",
			types.HLSSessionOptions{},
		},
		{
			"repeated parameter uses the first value",
			"mediaURL=http://example.com/a.mkv&bitrate=1M&bitrate=9M&ttl=2h&ttl=5m",
			types.HLSSessionOptions{Bitrate: "1M", TTL: "2h"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &fakeProber{}
			rec := serve(t, newHandlerWithProber(t, p), http.MethodGet, "/hlsv2/s1/master.m3u8?"+c.query, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; want 200", rec.Code)
			}
			if p.lastHLSOpts != c.want {
				t.Errorf("StartHLS opts = %+v; want %+v", p.lastHLSOpts, c.want)
			}
		})
	}
}

func TestHandlerHLS_MasterErrorStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
	}{
		{"invalid override → 400", fmt.Errorf("hls: %w: bitrate \"300M\": out of range", types.ErrInvalidHLSOption), http.StatusBadRequest},
		{"any other StartHLS error stays 500", errors.New("hls: too many concurrent sessions (limit 64)"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &fakeProber{startHLSErr: c.err}
			rec := serve(t, newHandlerWithProber(t, p), http.MethodGet, "/hlsv2/s1/master.m3u8?mediaURL=http://example.com/a.mkv&bitrate=300M", nil)
			if rec.Code != c.code {
				t.Errorf("status = %d; want %d", rec.Code, c.code)
			}
			if !strings.Contains(rec.Body.String(), c.err.Error()) {
				t.Errorf("body %q does not carry the error message %q", rec.Body.String(), c.err.Error())
			}
		})
	}
}

// Override params on a segment/playlist request are not forwarded anywhere:
// only master.m3u8 reaches StartHLS.
func TestHandlerHLS_OverridesOnlyOnMaster(t *testing.T) {
	p := &fakeProber{}
	serve(t, newHandlerWithProber(t, p), http.MethodGet, "/hlsv2/s1/seg0.ts?bitrate=1M&ttl=5", nil)
	if p.lastHLSOpts != (types.HLSSessionOptions{}) {
		t.Errorf("segment request reached StartHLS with %+v", p.lastHLSOpts)
	}
}

// A segment that failed because of its source is answered 502/504 with the
// reason, so a client can tell it from a missing segment (still 404).
func TestHandlerHLS_SegmentErrorStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
	}{
		{"source failed → 502", fmt.Errorf("hls: transcode seg3.ts: %w: upstream returned 429 Too Many Requests: exit status 1", types.ErrHLSSourceFailed), http.StatusBadGateway},
		{"segment timed out → 504", fmt.Errorf("hls: transcode seg3.ts: %w: upstream timed out: exit status 1", types.ErrHLSSegmentTimeout), http.StatusGatewayTimeout},
		{"unknown session stays 404", fmt.Errorf("hls: %w %q", types.ErrHLSSessionNotFound, "s1"), http.StatusNotFound},
		{"any other error stays 404", errors.New("hls: transcode seg3.ts: exit status 1"), http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &fakeProber{hlsFileErr: c.err}
			rec := serve(t, newHandlerWithProber(t, p), http.MethodGet, "/hlsv2/s1/seg3.ts", nil)
			if rec.Code != c.code {
				t.Errorf("status = %d; want %d", rec.Code, c.code)
			}
			if !strings.Contains(rec.Body.String(), c.err.Error()) {
				t.Errorf("body %q does not carry the error message %q", rec.Body.String(), c.err.Error())
			}
		})
	}
}
