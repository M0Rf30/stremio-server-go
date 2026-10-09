// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package streamproxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// hlsNewHandler builds a Handler with a deterministic external base URL.
func hlsNewHandler() *Handler {
	return New(Config{PublicURL: "https://ext.example"})
}

// hlsNewOpts returns Options targeting the standard test manifest URL.
func hlsNewOpts() *Options {
	return &Options{Dest: "https://cdn.example/live/index.m3u8"}
}

// TestHlsRewriteMasterPlaylist covers case (a): a master playlist containing an
// #EXT-X-STREAM-INF tag whose following URI line must be rewritten to
// /proxy/hls/manifest.m3u8 because it is a variant playlist.
func TestHlsRewriteMasterPlaylist(t *testing.T) {
	h := hlsNewHandler()
	r := httptest.NewRequest("GET", "/proxy/hls/manifest.m3u8", nil)
	opts := hlsNewOpts()

	playlist := "#EXTM3U\n" +
		"#EXT-X-VERSION:3\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360\n" +
		"variant_800k.m3u8\n"

	got := hlsRewrite(h, r, opts, playlist)

	// The variant URI must be proxied through /proxy/hls/manifest.m3u8.
	if !strings.Contains(got, "https://ext.example/proxy/hls/manifest.m3u8?d=") {
		t.Errorf("variant URI not rewritten to /proxy/hls:\n%s", got)
	}
	// Original relative URI must not appear.
	if strings.Contains(got, "variant_800k.m3u8\n") {
		t.Errorf("original variant URI still present:\n%s", got)
	}
	// Non-URI tags must be preserved verbatim.
	if !strings.Contains(got, "#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360") {
		t.Errorf("STREAM-INF tag was altered:\n%s", got)
	}
	if !strings.Contains(got, "#EXT-X-VERSION:3") {
		t.Errorf("VERSION tag missing:\n%s", got)
	}
}

// TestHlsRewriteMediaSegments covers case (b): a media playlist with relative
// segment URIs that must be rewritten to /proxy/stream.
func TestHlsRewriteMediaSegments(t *testing.T) {
	h := hlsNewHandler()
	r := httptest.NewRequest("GET", "/proxy/hls/manifest.m3u8", nil)
	opts := hlsNewOpts()

	playlist := "#EXTM3U\n" +
		"#EXT-X-VERSION:3\n" +
		"#EXT-X-TARGETDURATION:6\n" +
		"#EXTINF:6.000,\n" +
		"seg001.ts\n" +
		"#EXTINF:6.000,\n" +
		"seg002.ts\n" +
		"#EXT-X-ENDLIST\n"

	got := hlsRewrite(h, r, opts, playlist)

	// Segment URIs must be proxied through /proxy/stream/segment.ts (an
	// extension ffmpeg's HLS demuxer accepts).
	if !strings.Contains(got, "https://ext.example/proxy/stream/segment.ts?d=") {
		t.Errorf("segment URIs not rewritten to /proxy/stream/segment.ts:\n%s", got)
	}
	// Original relative URIs must not appear as bare lines.
	if strings.Contains(got, "\nseg001.ts\n") || strings.Contains(got, "\nseg002.ts\n") {
		t.Errorf("original segment URIs still present:\n%s", got)
	}
	// Non-URI tags must be preserved verbatim.
	if !strings.Contains(got, "#EXTINF:6.000,") {
		t.Errorf("EXTINF tag missing or altered:\n%s", got)
	}
	if !strings.Contains(got, "#EXT-X-ENDLIST") {
		t.Errorf("ENDLIST tag missing:\n%s", got)
	}
}

// TestHlsRewriteKey covers case (c): #EXT-X-KEY URI="..." must be rewritten
// to /proxy/stream while METHOD, IV, and other attributes are preserved.
func TestHlsRewriteKey(t *testing.T) {
	h := hlsNewHandler()
	r := httptest.NewRequest("GET", "/proxy/hls/manifest.m3u8", nil)
	opts := hlsNewOpts()

	playlist := "#EXT-X-KEY:METHOD=AES-128,URI=\"https://cdn.example/keys/key.bin\",IV=0x00000001\n"

	got := hlsRewrite(h, r, opts, playlist)

	// KEY URI must be proxied through /proxy/stream.
	if !strings.Contains(got, "https://ext.example/proxy/stream?d=") {
		t.Errorf("KEY URI not rewritten to /proxy/stream:\n%s", got)
	}
	// Original absolute KEY URI must not appear in the URI attribute.
	if strings.Contains(got, `URI="https://cdn.example/keys/key.bin"`) {
		t.Errorf("original KEY URI still present:\n%s", got)
	}
	// METHOD and IV must be preserved.
	if !strings.Contains(got, "METHOD=AES-128") {
		t.Errorf("METHOD attribute removed:\n%s", got)
	}
	if !strings.Contains(got, "IV=0x00000001") {
		t.Errorf("IV attribute removed:\n%s", got)
	}
}

// TestHlsRewriteMap covers case (d): #EXT-X-MAP URI="..." must be rewritten
// to /proxy/stream (initialization segment, not a playlist).
func TestHlsRewriteMap(t *testing.T) {
	h := hlsNewHandler()
	r := httptest.NewRequest("GET", "/proxy/hls/manifest.m3u8", nil)
	opts := hlsNewOpts()

	playlist := "#EXT-X-MAP:URI=\"init.mp4\",BYTERANGE=\"1024@0\"\n"

	got := hlsRewrite(h, r, opts, playlist)

	// MAP URI must be proxied through /proxy/stream/segment.mp4.
	if !strings.Contains(got, "https://ext.example/proxy/stream/segment.mp4?d=") {
		t.Errorf("MAP URI not rewritten to /proxy/stream/segment.mp4:\n%s", got)
	}
	// Original relative MAP URI must not appear.
	if strings.Contains(got, `URI="init.mp4"`) {
		t.Errorf("original MAP URI still present:\n%s", got)
	}
	// BYTERANGE must be preserved.
	if !strings.Contains(got, `BYTERANGE="1024@0"`) {
		t.Errorf("BYTERANGE attribute removed:\n%s", got)
	}
}

// TestHlsRewriteMediaAudio covers case (e): #EXT-X-MEDIA URI="audio.m3u8"
// must be rewritten to /proxy/hls/manifest.m3u8 because the URL is a playlist.
func TestHlsRewriteMediaAudio(t *testing.T) {
	h := hlsNewHandler()
	r := httptest.NewRequest("GET", "/proxy/hls/manifest.m3u8", nil)
	opts := hlsNewOpts()

	playlist := "#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",NAME=\"English\",DEFAULT=YES,URI=\"audio.m3u8\"\n"

	got := hlsRewrite(h, r, opts, playlist)

	// MEDIA URI referencing a .m3u8 must go through /proxy/hls/manifest.m3u8.
	if !strings.Contains(got, "https://ext.example/proxy/hls/manifest.m3u8?d=") {
		t.Errorf("MEDIA audio URI not rewritten to /proxy/hls:\n%s", got)
	}
	// Original relative URI must not appear.
	if strings.Contains(got, `URI="audio.m3u8"`) {
		t.Errorf("original MEDIA URI still present:\n%s", got)
	}
	// Other attributes must be preserved.
	if !strings.Contains(got, `TYPE=AUDIO`) {
		t.Errorf("TYPE attribute removed:\n%s", got)
	}
	if !strings.Contains(got, `GROUP-ID="audio"`) {
		t.Errorf("GROUP-ID attribute removed:\n%s", got)
	}
}

// TestHlsRewriteMediaNoExtension: rendition URIs without ".m3u8" (e.g.
// /playlist/1?type=audio) are still media playlists and must use the HLS
// endpoint, otherwise their keys/segments are never rewritten.
func TestHlsRewriteMediaNoExtension(t *testing.T) {
	h := hlsNewHandler()
	r := httptest.NewRequest("GET", "/proxy/hls/manifest.m3u8", nil)
	opts := hlsNewOpts()
	playlist := "#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",URI=\"/playlist/1?type=audio&rendition=ita\"\n" +
		"#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=1,URI=\"/playlist/1?type=iframe\"\n"
	got := hlsRewrite(h, r, opts, playlist)
	if strings.Contains(got, "/proxy/stream?") || strings.Count(got, "/proxy/hls/manifest.m3u8?d=") != 2 {
		t.Errorf("extension-less rendition URIs not routed via hls:\n%s", got)
	}
}

// TestHlsSegmentEndpoint: proxied segment URLs must end in an extension
// ffmpeg's HLS demuxer accepts (extension_picky, default since ffmpeg 7.1).
func TestHlsSegmentEndpoint(t *testing.T) {
	for abs, want := range map[string]string{
		"https://cdn.example/a/seg001.ts":           "/proxy/stream/segment.ts",
		"https://cdn.example/a/chunk.M4S?x=1":       "/proxy/stream/segment.m4s",
		"https://cdn.example/a/0000-0001":           "/proxy/stream/segment.ts", // extension-less
		"https://cdn.example/a/frame.jpg":           "/proxy/stream/segment.ts", // disguised segment
		"https://cdn.example/a/seg.html?token=abc":  "/proxy/stream/segment.ts",
		"https://cdn.example/a/audio.aac":           "/proxy/stream/segment.aac",
		"https://cdn.example/sub/subs-0000.vtt?t=1": "/proxy/stream/segment.vtt",
		"https://cdn.example/a/b.ts/../c":           "/proxy/stream/segment.ts",
		"https://cdn.example/a/path.with.dots/file": "/proxy/stream/segment.ts",
	} {
		if got := hlsSegmentEndpoint(abs, "ts"); got != want {
			t.Errorf("hlsSegmentEndpoint(%s) = %s, want %s", abs, got, want)
		}
	}
	if got := hlsSegmentEndpoint("https://cdn.example/init", "mp4"); got != "/proxy/stream/segment.mp4" {
		t.Errorf("default not applied: %s", got)
	}
	// fMP4 playlists (#EXT-X-MAP) label extension-less segments .mp4 so the
	// label matches the format ffmpeg detects.
	h := hlsNewHandler()
	r := httptest.NewRequest("GET", "/proxy/hls/manifest.m3u8", nil)
	got := hlsRewrite(h, r, hlsNewOpts(), "#EXT-X-MAP:URI=\"init\"\n#EXTINF:1,\nchunk-1\n")
	if strings.Count(got, "/proxy/stream/segment.mp4?d=") != 2 {
		t.Errorf("fMP4 segments not labelled .mp4:\n%s", got)
	}
}

// TestHlsSegmentURLServes: the rewritten /proxy/stream/segment.<ext> URL is
// served by Route exactly like /proxy/stream.
func TestHlsSegmentURLServes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("SEGMENTDATA"))
	}))
	defer upstream.Close()
	h := New(Config{PublicURL: "https://ext.example"})
	defer h.Close()
	r := httptest.NewRequest("GET", "/proxy/hls/manifest.m3u8", nil)
	got := hlsRewrite(h, r, &Options{Dest: upstream.URL + "/live/index.m3u8"}, "#EXTINF:1,\n0000-0001\n")
	u, err := url.Parse(strings.TrimSpace(strings.Split(got, "\n")[1]))
	if err != nil || u.Path != "/proxy/stream/segment.ts" {
		t.Fatalf("unexpected segment URL %q (%v)", got, err)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", u.RequestURI(), nil)
	if !h.Route(w, req, strings.Split(strings.Trim(u.Path, "/"), "/")) {
		t.Fatal("Route did not handle /proxy/stream/segment.ts")
	}
	if w.Code != http.StatusOK || w.Body.String() != "SEGMENTDATA" {
		t.Errorf("serve: %d %q", w.Code, w.Body.String())
	}
}
