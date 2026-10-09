// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package streamproxy

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// benchPlaylist builds a media playlist shaped like real VOD/live sources:
// AES-128 key, absolute extension-less segment URLs carrying signed tokens.
func benchPlaylist(segments int) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n")
	b.WriteString("#EXT-X-KEY:METHOD=AES-128,URI=\"/storage/enc.key\",IV=0x00000000000000000000000000000001\n")
	for i := range segments {
		fmt.Fprintf(&b, "#EXTINF:4.000000,\nhttps://sc-u11-01.cdn.example/hls/3/8/cf/8cf686c9-dd4a-4275-b85a/video/480p/%04d-%04d?token=R8v-PrGBJbrHdTSVOptAXw&expires=1796730483\n", i, i+1)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

func benchHLSRewrite(b *testing.B, segments int) {
	h := New(Config{PublicURL: "https://ext.example", Password: "pw"})
	defer h.Close()
	r := httptest.NewRequest("GET", "/proxy/hls/manifest.m3u8", nil)
	opts := &Options{Dest: "https://cdn.example/playlist/1?type=video&rendition=480p", APIPassword: "pw",
		ReqHeaders: http.Header{"Referer": {"https://player.example/"}, "Origin": {"https://player.example"}}}
	pl := benchPlaylist(segments)
	b.SetBytes(int64(len(pl)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = hlsRewrite(h, r, opts, pl)
	}
}

// BenchmarkHLSRewriteVOD: a full movie playlist (~2k segments), rewritten
// once per player load.
func BenchmarkHLSRewriteVOD(b *testing.B) { benchHLSRewrite(b, 2000) }

// BenchmarkHLSRewriteLive: a sliding live window, rewritten every few seconds.
func BenchmarkHLSRewriteLive(b *testing.B) { benchHLSRewrite(b, 10) }

// benchRegistry writes n definitions (every one with a match pattern) and
// returns a loaded registry.
func benchRegistry(b *testing.B, n int) *defRegistry {
	// Registry load logs ("local extractors loaded") would interleave with
	// the benchmark result lines and corrupt benchstat input.
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	b.Cleanup(func() { slog.SetDefault(prev) })
	dir := b.TempDir()
	var defs []string
	for i := range n {
		defs = append(defs, fmt.Sprintf(`"host%02d":{"match":"^https?://(www\\.)?embed%02d\\.example/[ef]/","steps":[{"fetch":"{input}"},{"regex":"src:\\s*\"([^\"]+)\"","set":"src"}],"result":{"url":"{src}","endpoint":"stream"}}`, i, i))
	}
	doc := `{"version":1,"extractors":{` + strings.Join(defs, ",") + `}}`
	if err := os.WriteFile(filepath.Join(dir, "extractors.json"), []byte(doc), 0o600); err != nil {
		b.Fatal(err)
	}
	reg := newDefRegistry(Config{AppPath: dir})
	if _, ok := reg.lookup("host00"); !ok {
		b.Fatal("definitions not loaded")
	}
	b.Cleanup(reg.close)
	return reg
}

// BenchmarkDefMatchMiss is the cost /proxy/stream pays on every segment
// request that is not an embed page (the overwhelmingly common case).
func BenchmarkDefMatchMiss(b *testing.B) {
	for _, n := range []int{0, 10, 50} {
		b.Run(fmt.Sprintf("defs=%d", n), func(b *testing.B) {
			reg := benchRegistry(b, n)
			dest := "https://sc-u11-01.cdn.example/hls/3/8/cf/video/480p/0001-0002?token=R8v&expires=1796730483"
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, _, ok := reg.match(dest); ok {
					b.Fatal("unexpected match")
				}
			}
		})
	}
}

// BenchmarkDefMatchMissParallel: the same miss under concurrent segment
// fetches (players prefetch several segments at once; the registry lock is
// shared by every request).
func BenchmarkDefMatchMissParallel(b *testing.B) {
	reg := benchRegistry(b, 10)
	dest := "https://sc-u11-01.cdn.example/hls/3/8/cf/video/480p/0001-0002?token=R8v&expires=1796730483"
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _, _ = reg.match(dest)
		}
	})
}

// BenchmarkHLSSegmentEndpoint isolates the per-segment endpoint choice.
func BenchmarkHLSSegmentEndpoint(b *testing.B) {
	abs := "https://sc-u11-01.cdn.example/hls/3/8/cf/8cf686c9/video/480p/0001-0002?token=R8v-PrGBJbrHdTSVOptAXw&expires=1796730483"
	b.ReportAllocs()
	for b.Loop() {
		_ = hlsSegmentEndpoint(abs, "ts")
	}
}
