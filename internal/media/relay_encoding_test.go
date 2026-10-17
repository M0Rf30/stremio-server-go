// SPDX-FileCopyrightText: 2026 The rivulet-engine Authors
//
// SPDX-License-Identifier: MIT

package media

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestMediaRelayInspectsCompressedBodies: ffmpeg inflates gzip/deflate
// bodies before probing, so the relay asks upstream for identity and, when
// it gets a compressed body anyway, decodes it, sniffs what ffmpeg would
// see and passes it on uncompressed. A hidden DASH manifest is refused, an
// HLS playlist gets its nested URIs rewritten, media arrives decoded, a
// 2xx body that doesn't decode is refused (502), and codings ffmpeg doesn't decode
// pass through raw. Only a 2xx is decoded; an error status passes through
// unchanged, and a 206 is decoded only from byte 0.
func TestMediaRelayInspectsCompressedBodies(t *testing.T) {
	compress := func(enc, s string) string {
		var b bytes.Buffer
		var w io.WriteCloser
		if enc == "gzip" {
			w = gzip.NewWriter(&b)
		} else {
			w = zlib.NewWriter(&b)
		}
		_, _ = io.WriteString(w, s)
		_ = w.Close()
		return b.String()
	}
	// The MPD root sits in the first 512 decoded bytes, where looksLikeDASH
	// finds it; the padding makes the encoders emit real compressed blocks
	// rather than storing the bytes (in which "<MPD" would stay readable).
	mpd := `<?xml version="1.0"?><MPD profiles="urn:mpeg:dash:profile:isoff-live:2011"><BaseURL>http://169.254.169.254/</BaseURL></MPD>` +
		"<!-- " + strings.Repeat("padding ", 512) + "-->"
	// Behind 600 bytes of whitespace no "<MPD" is in the first 512 decoded
	// bytes, so only the deeper XML look sees it.
	spacedMPD := strings.Repeat(" ", 600) + mpd
	m3u8 := "#EXTM3U\n#EXTINF:4,\nhttp://169.254.169.254/seg.ts\n"
	media := "\x47\x40\x00\x10" + strings.Repeat("\xff", 1024)

	for _, enc := range []string{"gzip", "deflate"} {
		if strings.Contains(compress(enc, mpd), "<MPD") || strings.Contains(compress(enc, spacedMPD), "<MPD") {
			t.Fatalf("%s output still contains a readable <MPD", enc)
		}
	}

	var sawEncoding []string
	gzMedia := compress("gzip", media)
	// A gzip stream whose CRC trailer is wrong: ffmpeg returns the decoded
	// bytes and ends cleanly.
	badTrailer := []byte(gzMedia)
	badTrailer[len(badTrailer)-8] ^= 0xff
	decodedMedia := func(t *testing.T, resp *http.Response, got string) {
		if got != media || resp.Header.Get("Content-Encoding") != "" {
			t.Errorf("want the decoded body without Content-Encoding; got %d bytes, CE %q", len(got), resp.Header.Get("Content-Encoding"))
		}
		// Content-Length, if any, is the relay's own for the decoded body,
		// never the upstream's compressed length.
		if resp.ContentLength != -1 && resp.ContentLength != int64(len(got)) {
			t.Errorf("Content-Length = %d for a %d-byte body", resp.ContentLength, len(got))
		}
	}

	cases := []struct {
		name, ce, body, rng string
		status              int               // upstream status; 0 = 200
		hdr                 map[string]string // extra upstream headers
		wantStatus          int
		check               func(t *testing.T, resp *http.Response, got string)
	}{
		{"deflate DASH", "deflate", compress("deflate", mpd), "", 0, nil, http.StatusUnsupportedMediaType, nil},
		{"gzip DASH with Range", "GZIP", compress("gzip", mpd), "bytes=0-", 0, nil, http.StatusUnsupportedMediaType, nil},
		{"gzip DASH behind 600 spaces with Range", "gzip", compress("gzip", spacedMPD), "bytes=0-", 0, nil, http.StatusUnsupportedMediaType, nil},
		{"gzip DASH in a 206 from 0", "gzip", compress("gzip", mpd), "bytes=0-", http.StatusPartialContent,
			map[string]string{"Content-Range": "bytes 0-99/100"}, http.StatusUnsupportedMediaType, nil},
		{"undecodable gzip", "gzip", "not gzip at all", "", 0, nil, http.StatusBadGateway, nil},
		{"gzip 206 from the middle is refused", "gzip", gzMedia[10:], "bytes=10-", http.StatusPartialContent,
			map[string]string{"Content-Range": fmt.Sprintf("bytes 10-%d/%d", len(gzMedia)-1, len(gzMedia))}, http.StatusBadGateway, nil},
		{"empty gzip 200", "gzip", "", "", 0, nil, http.StatusOK, nil},
		{"gzip 204", "gzip", "", "", http.StatusNoContent, nil, http.StatusNoContent, nil},
		// "|" separates header lines: ffmpeg inflates if any line says gzip.
		{"gzip DASH on a second header line", "identity|gzip", compress("gzip", mpd), "", 0, nil, http.StatusUnsupportedMediaType, nil},
		{"gzip HLS playlist with Range", "gzip", compress("gzip", m3u8), "bytes=0-", 0, nil, http.StatusOK, func(t *testing.T, _ *http.Response, got string) {
			if strings.Contains(got, "169.254.169.254") || !strings.Contains(got, "/r/") {
				t.Errorf("nested URI not rewritten: %q", got)
			}
		}},
		{"gzip media with Range", "gzip", gzMedia, "bytes=0-", 0, nil, http.StatusOK, decodedMedia},
		{"gzip 206 from 0 becomes a plain 200", "gzip", gzMedia, "bytes=0-", http.StatusPartialContent,
			map[string]string{"Content-Range": "bytes 0-999/1000", "Accept-Ranges": "bytes"}, http.StatusOK,
			func(t *testing.T, resp *http.Response, got string) {
				decodedMedia(t, resp, got)
				if resp.Header.Get("Content-Range") != "" || resp.Header.Get("Accept-Ranges") != "" {
					t.Errorf("compressed-byte range headers kept: %q / %q", resp.Header.Get("Content-Range"), resp.Header.Get("Accept-Ranges"))
				}
			}},
		{"bad gzip trailer ends cleanly", "gzip", string(badTrailer), "", 0, nil, http.StatusOK, decodedMedia},
		{"empty 416 keeps its status", "gzip", "", "bytes=5000-", http.StatusRequestedRangeNotSatisfiable, nil, http.StatusRequestedRangeNotSatisfiable, nil},
		// A broken gzip body on an error status is never decoded: the status
		// passes through, as do the body and its Content-Encoding.
		{"gzip 404 with a broken body keeps its 404", "gzip", "not found", "", http.StatusNotFound, nil, http.StatusNotFound,
			func(t *testing.T, resp *http.Response, got string) {
				if got != "not found" || resp.Header.Get("Content-Encoding") != "gzip" {
					t.Errorf("error response altered: %q, CE %q", got, resp.Header.Get("Content-Encoding"))
				}
			}},
		{"gzip 500 with a broken body keeps its 500", "gzip", "\x1f\x8b broken", "", http.StatusInternalServerError, nil, http.StatusInternalServerError, nil},
		{"br passes raw", "br", media, "", 0, nil, http.StatusOK, func(t *testing.T, resp *http.Response, got string) {
			if got != media || resp.Header.Get("Content-Encoding") != "br" {
				t.Errorf("want the raw body with CE br; got %d bytes, CE %q", len(got), resp.Header.Get("Content-Encoding"))
			}
		}},
		{"identity media", "identity", media, "", 0, nil, http.StatusOK, nil},
		{"plain media", "", media, "", 0, nil, http.StatusOK, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				sawEncoding = append(sawEncoding, req.Header.Get("Accept-Encoding"))
				w.Header().Set("Content-Type", "application/octet-stream")
				for _, ce := range strings.Split(c.ce, "|") {
					if ce != "" {
						w.Header().Add("Content-Encoding", ce)
					}
				}
				for k, v := range c.hdr {
					w.Header().Set(k, v)
				}
				if c.status != 0 {
					w.WriteHeader(c.status)
				}
				_, _ = io.WriteString(w, c.body)
			}))
			defer upstream.Close()
			r := newMediaRelay(false)
			relayURL, err := r.register(upstream.URL + "/x")
			if err != nil {
				t.Fatalf("register: %v", err)
			}
			req, _ := http.NewRequest(http.MethodGet, relayURL, nil)
			if c.rng != "" {
				req.Header.Set("Range", c.rng)
			}
			// Asking for gzip ourselves stops the transport's transparent
			// decoding, so the test sees what the relay sent; the relay must
			// still ask upstream for identity.
			req.Header.Set("Accept-Encoding", "gzip, deflate")
			resp, err := http.DefaultTransport.RoundTrip(req)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			got, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != c.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, c.wantStatus)
			}
			if c.check != nil {
				c.check(t, resp, string(got))
			} else if c.wantStatus == http.StatusOK && string(got) != c.body {
				t.Errorf("body altered: got %d bytes, want %d", len(got), len(c.body))
			}
		})
	}
	for _, ae := range sawEncoding {
		if ae != "identity" {
			t.Errorf("upstream saw Accept-Encoding %q, want identity", ae)
		}
	}
}

// TestMediaRelayCompressedUpstreamBreakAborts: a decoding error ends the
// body cleanly (as in ffmpeg), but an upstream connection that breaks
// partway through a gzip body is still an upstream failure: the relay
// aborts and records it.
func TestMediaRelayCompressedUpstreamBreakAborts(t *testing.T) {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	_, _ = io.WriteString(gz, strings.Repeat("\x47\x40\x00\x10", 64<<10))
	_ = gz.Close()
	half := b.Bytes()[:b.Len()/2]

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(half)
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	defer upstream.Close()
	r := newMediaRelay(false)
	relayURL, err := r.register(upstream.URL + "/x")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	began := time.Now()
	resp, err := http.Get(relayURL) //nolint:gosec // our own loopback relay
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("relay delivered a clean end for a broken upstream")
	}
	if f, ok := r.failureSince(relayURL, began); !ok || f.reason != "upstream body ended early" {
		t.Errorf("failureSince = %+v, %v; want the upstream break recorded", f, ok)
	}
}

// TestMediaRelaySelfEntryCompressedBodies: a self entry (/proxy, /ftp,
// /yt fetched through selfClient) is read by ffmpeg through the relay like
// any other, so it gets the same identity request and decoding: a gzip
// DASH manifest is refused, and a gzip playlist is decoded and rewritten.
func TestMediaRelaySelfEntryCompressedBodies(t *testing.T) {
	gz := func(s string) []byte {
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		_, _ = io.WriteString(w, s)
		_ = w.Close()
		return b.Bytes()
	}
	mpd := `<?xml version="1.0"?><MPD><BaseURL>http://169.254.169.254/</BaseURL></MPD>` +
		"<!-- " + strings.Repeat("padding ", 512) + "-->"
	m3u8 := "#EXTM3U\n#EXTINF:4,\nhttp://169.254.169.254/seg.ts\n"
	var sawEncoding []string
	self := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		sawEncoding = append(sawEncoding, req.Header.Get("Accept-Encoding"))
		w.Header().Set("Content-Encoding", "gzip")
		if strings.HasSuffix(req.URL.Path, "/manifest") {
			_, _ = w.Write(gz(mpd))
			return
		}
		_, _ = w.Write(gz(m3u8))
	}))
	defer self.Close()

	r := newMediaRelay(false)
	get := func(path string) (int, string) {
		t.Helper()
		relayURL, err := r.registerAs(self.URL+path, self.URL)
		if err != nil {
			t.Fatalf("registerAs: %v", err)
		}
		req, _ := http.NewRequest(http.MethodGet, relayURL, nil)
		req.Header.Set("Range", "bytes=0-")
		req.Header.Set("Accept-Encoding", "gzip, deflate")
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := get("/proxy/d=x/manifest"); code != http.StatusUnsupportedMediaType {
		t.Errorf("gzip DASH via self /proxy = %d, want 415", code)
	}
	code, body := get("/proxy/d=x/list")
	if code != http.StatusOK || !strings.HasPrefix(body, "#EXTM3U") || strings.Contains(body, "169.254.169.254") {
		t.Errorf("gzip playlist via self /proxy = %d %q, want it decoded and rewritten", code, body)
	}
	for _, ae := range sawEncoding {
		if ae != "identity" {
			t.Errorf("self origin saw Accept-Encoding %q, want identity", ae)
		}
	}
}
