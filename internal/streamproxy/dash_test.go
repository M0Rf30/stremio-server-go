package streamproxy

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// dashTestMPD is a minimal MPEG-DASH MPD document that exercises all rewrite paths:
//   - <BaseURL>       → CharData URL rewrite (base64 proxy destination)
//   - <SegmentTemplate> → attribute rewrite preserving $…$ placeholders
//   - <SegmentURL>    → attribute rewrite with base64 proxy destination
//   - <Initialization> → sourceURL attribute rewrite with base64 proxy destination
const dashTestMPD = `<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT10S">
  <Period>
    <AdaptationSet mimeType="video/mp4">
      <BaseURL>https://cdn.example/dash/</BaseURL>
      <Representation id="video" bandwidth="1500000" codecs="avc1.42E01E">
        <SegmentTemplate initialization="init-$RepresentationID$.m4s" media="seg-$RepresentationID$-$Number$.m4s" startNumber="1" duration="4"/>
      </Representation>
      <Representation id="audio" bandwidth="128000" codecs="mp4a.40.2">
        <SegmentList>
          <Initialization sourceURL="init.mp4" range="0-999"/>
          <SegmentURL media="chunk1.m4s"/>
          <SegmentURL media="chunk2.m4s" index="idx2.sidx"/>
        </SegmentList>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

// dashNewTestHandler creates a Handler suitable for rewrite tests.
// PublicURL is fixed so externalBase is deterministic regardless of request host.
func dashNewTestHandler() *Handler {
	return New(Config{PublicURL: "https://ext.example"})
}

// dashRewriteResult runs dashRewrite with a standard test request and opts.Dest.
func dashRewriteResult(dest string) (h *Handler, out string) {
	h = dashNewTestHandler()
	req := httptest.NewRequest("GET", "/proxy/mpd/manifest.m3u8?d="+dest, nil)
	opts := &Options{Dest: dest}
	raw := dashRewrite(h, req, opts, []byte(dashTestMPD))
	return h, string(raw)
}

// ---------------------------------------------------------------------------
// BaseURL rewriting
// ---------------------------------------------------------------------------

func TestDashRewriteBaseURL(t *testing.T) {
	const dest = "https://origin.example/stream/master.mpd"
	_, out := dashRewriteResult(dest)

	// The raw CDN URL must no longer appear as BaseURL text content.
	if strings.Contains(out, ">https://cdn.example/dash/<") {
		t.Errorf("BaseURL still contains original CDN URL; output:\n%s", out)
	}

	// The rewritten BaseURL must route through the proxy stream endpoint.
	if !strings.Contains(out, "https://ext.example/proxy/stream?d=") {
		t.Errorf("BaseURL not rewritten through proxy; output:\n%s", out)
	}

	// Verify the exact base64-encoded destination appears.
	abs := resolveURL(dest, "https://cdn.example/dash/")
	encoded := base64.RawURLEncoding.EncodeToString([]byte(abs))
	if !strings.Contains(out, "proxy/stream?d="+encoded) {
		t.Errorf("BaseURL proxy URL has wrong base64 destination (want d=%s); output:\n%s", encoded, out)
	}
}

// ---------------------------------------------------------------------------
// SegmentTemplate placeholder preservation
// ---------------------------------------------------------------------------

func TestDashRewriteSegmentTemplatePlaceholders(t *testing.T) {
	const dest = "https://origin.example/stream/master.mpd"
	_, out := dashRewriteResult(dest)

	// $Number$ must survive in the media attribute so the DASH player can
	// perform template expansion before requesting each segment.
	if !strings.Contains(out, "$Number$") {
		t.Errorf("SegmentTemplate $Number$ placeholder was lost; output:\n%s", out)
	}
	if !strings.Contains(out, "$RepresentationID$") {
		t.Errorf("SegmentTemplate $RepresentationID$ placeholder was lost; output:\n%s", out)
	}

	// The template must be wrapped with the proxy stream URL prefix.
	if !strings.Contains(out, "https://ext.example/proxy/stream?d=") {
		t.Errorf("SegmentTemplate attributes not wrapped with proxy URL; output:\n%s", out)
	}
}

func TestDashRewriteSegmentTemplateURLShape(t *testing.T) {
	const dest = "https://origin.example/stream/master.mpd"
	_, out := dashRewriteResult(dest)

	// The plain (non-base64) encoding means the d= value starts with
	// "https%3A%2F%2F" (percent-encoded scheme) rather than a base64 blob.
	// Confirm the proxy stream prefix is followed by the percent-encoded origin.
	proxyPrefix := "https://ext.example/proxy/stream?d=https%3A%2F%2Forigin.example"
	if !strings.Contains(out, proxyPrefix) {
		t.Errorf("SegmentTemplate proxy URL does not begin with plain-encoded origin; output:\n%s", out)
	}

	// Confirm the initialization template also points through the proxy.
	if !strings.Contains(out, "init-$RepresentationID$.m4s") {
		// The placeholder must still be there; the surrounding proxy URL wraps it.
		t.Errorf("SegmentTemplate initialization placeholder missing; output:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// SegmentURL base64 rewriting
// ---------------------------------------------------------------------------

func TestDashRewriteSegmentURLBase64(t *testing.T) {
	const dest = "https://origin.example/stream/master.mpd"
	_, out := dashRewriteResult(dest)

	// chunk1.m4s must not appear as a raw attribute value.
	if strings.Contains(out, `media="chunk1.m4s"`) {
		t.Errorf("SegmentURL media still contains raw chunk1.m4s; output:\n%s", out)
	}

	// The resolved absolute URL must appear base64url-encoded in d=.
	abs := resolveURL(dest, "chunk1.m4s")
	encoded := base64.RawURLEncoding.EncodeToString([]byte(abs))
	if !strings.Contains(out, "proxy/stream?d="+encoded) {
		t.Errorf("SegmentURL media not base64-encoded (want d=%s); output:\n%s", encoded, out)
	}
}

func TestDashRewriteSegmentURLIndexAttribute(t *testing.T) {
	const dest = "https://origin.example/stream/master.mpd"
	_, out := dashRewriteResult(dest)

	// The index attribute on chunk2 must also be rewritten.
	if strings.Contains(out, `index="idx2.sidx"`) {
		t.Errorf("SegmentURL index attribute not rewritten; output:\n%s", out)
	}
	abs := resolveURL(dest, "idx2.sidx")
	encoded := base64.RawURLEncoding.EncodeToString([]byte(abs))
	if !strings.Contains(out, "proxy/stream?d="+encoded) {
		t.Errorf("SegmentURL index not base64-encoded (want d=%s); output:\n%s", encoded, out)
	}
}

// ---------------------------------------------------------------------------
// Initialization sourceURL rewriting
// ---------------------------------------------------------------------------

func TestDashRewriteInitializationSourceURL(t *testing.T) {
	const dest = "https://origin.example/stream/master.mpd"
	_, out := dashRewriteResult(dest)

	// sourceURL="init.mp4" must be rewritten; range="0-999" must be untouched.
	if strings.Contains(out, `sourceURL="init.mp4"`) {
		t.Errorf("Initialization sourceURL not rewritten; output:\n%s", out)
	}
	if !strings.Contains(out, `range="0-999"`) {
		t.Errorf("Initialization range attribute was unexpectedly modified; output:\n%s", out)
	}
	abs := resolveURL(dest, "init.mp4")
	encoded := base64.RawURLEncoding.EncodeToString([]byte(abs))
	if !strings.Contains(out, "proxy/stream?d="+encoded) {
		t.Errorf("Initialization sourceURL not base64-encoded (want d=%s); output:\n%s", encoded, out)
	}
}

// ---------------------------------------------------------------------------
// Output XML validity
// ---------------------------------------------------------------------------

func TestDashRewriteOutputIsValidXML(t *testing.T) {
	const dest = "https://origin.example/stream/master.mpd"
	_, out := dashRewriteResult(dest)

	dec := xml.NewDecoder(strings.NewReader(out))
	for {
		_, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Errorf("rewritten output is not valid XML: %v\noutput:\n%s", err, out)
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Fallback on malformed input
// ---------------------------------------------------------------------------

func TestDashRewriteInvalidXMLFallback(t *testing.T) {
	h := dashNewTestHandler()
	req := httptest.NewRequest("GET", "/proxy/mpd/manifest.m3u8?d=https://origin.example/master.mpd", nil)
	opts := &Options{Dest: "https://origin.example/master.mpd"}

	garbage := []byte("<<not valid xml at all>>")
	out := dashRewrite(h, req, opts, garbage)
	if !bytes.Equal(out, garbage) {
		t.Errorf("expected original bytes returned on invalid XML; got %q", out)
	}
}
