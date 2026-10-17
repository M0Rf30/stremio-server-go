// SPDX-FileCopyrightText: 2026 The rivulet-engine Authors
//
// SPDX-License-Identifier: MIT

package media

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf16"
)

// TestMediaRelayRewritesSniffedPlaylist: an HLS playlist served as
// octet-stream without a .m3u8 path must still be rewritten through the relay.
func TestMediaRelayRewritesSniffedPlaylist(t *testing.T) {
	const pl = "\xef\xbb\xbf\n#EXTM3U\n#EXTINF:4,\nhttp://169.254.169.254/latest/seg.ts\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(pl))
	}))
	defer upstream.Close()

	r := newMediaRelay(false)
	relayURL, err := r.register(upstream.URL + "/stream")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	resp, err := http.Get(relayURL) //nolint:gosec
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if strings.Contains(string(got), "169.254.169.254") || !strings.Contains(string(got), "/r/") {
		t.Errorf("nested URI not rewritten: %q", got)
	}
}

// TestMediaRelayRefusesDASH: DASH manifests are not rewritable, so refuse.
func TestMediaRelayRefusesDASH(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(`<?xml version="1.0"?><MPD><BaseURL>http://169.254.169.254/</BaseURL></MPD>`))
	}))
	defer upstream.Close()

	r := newMediaRelay(false)
	relayURL, err := r.register(upstream.URL + "/x")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	resp, err := http.Get(relayURL) //nolint:gosec
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", resp.StatusCode)
	}
}

func TestParseProbeOutputRejectsAbsurdDuration(t *testing.T) {
	for _, d := range []string{"NaN", "Inf", "-5", "0", "1e12"} {
		res := parseProbeOutput([]byte(`{"format":{"duration":"` + d + `"},"streams":[]}`))
		if res.duration != 0 {
			t.Errorf("duration %q parsed to %v, want 0", d, res.duration)
		}
	}
	if res := parseProbeOutput([]byte(`{"format":{"duration":"120.5"},"streams":[]}`)); res.duration != 120.5 {
		t.Errorf("valid duration = %v", res.duration)
	}
}

// TestMediaRelayRefusesRedirectedDASH: a DASH manifest is refused however
// it is disguised. The redirect sources carry no hint; only the final
// response does: its .mpd path (with a body that can't be sniffed), its
// Content-Type, an MPD root behind a prolog longer than the deep sniff
// window, a lowercase root, a root behind more whitespace than the first
// peek holds, or no root at all within the window (ffmpeg reads further
// than the relay does, so an undecided XML body fails closed). Other XML,
// UTF-16 XML (which ffmpeg can't probe as DASH) and media bodies still pass
// through intact.
func TestMediaRelayRefusesRedirectedDASH(t *testing.T) {
	const root = `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><BaseURL>http://169.254.169.254/</BaseURL></MPD>`
	decl := `<?xml version="1.0" encoding="UTF-8"?>` + "\n"
	comment := func(n int) string { return "<!-- " + strings.Repeat("x", n) + " -->\n" }
	shortProlog := decl + comment(2048)
	longProlog := decl + comment(17<<10)
	if len(shortProlog) <= relaySniffBytes || len(longProlog) <= relayXMLSniffBytes {
		t.Fatalf("prologs must exceed the sniff windows: %d, %d", len(shortProlog), len(longProlog))
	}
	xmlOther := shortProlog + `<tt xmlns="http://www.w3.org/ns/ttml"><body/></tt>`
	htmlErr := "<!DOCTYPE html>\n<html><head><title>502</title></head><body>Bad gateway</body></html>"
	media := "\x47\x40\x00\x10" + strings.Repeat("\xff", 2048)
	// encoding/xml ends this DOCTYPE inside the PI and sees <tt> as the
	// root; libxml2 parses the PI and finds MPD.
	piDesync := shortProlog + `<!DOCTYPE MPD [<?pi >> <tt> ?>]>` + root
	deepMention := shortProlog + `<!-- <mpd> -->` + `<tt xmlns="http://www.w3.org/ns/ttml"/>`
	// dash_probe's av_stristr stops at the NUL beside the first UTF-16
	// '<', so ffmpeg never content-probes this as DASH.
	utf16MPD := utf16LE("\ufeff" + shortProlog + root)
	sp := func(n int) string { return strings.Repeat(" ", n) }
	slice := "<\x9a\x01\x02" + strings.Repeat("\xff\x00", 4096)
	// A control byte other than NUL doesn't exempt a body from the walk.
	ctrlLong := decl + "<!-- \x01 -->" + comment(17<<10) + root
	// UTF-7 with spaces around '=': libxml2 decodes the "+ADw-" escapes to
	// a comment and an MPD root, encoding/xml sees <tt>. The raw "<MPD"
	// that dash_probe needs sits past the 16 KiB window.
	utf7Padded := `<?xml version="1.0" encoding = "UTF-7"?>` +
		`+ADwAIQAtAC0-<tt>-->+ADw-MPD profiles="urn:mpeg:dash:profile:isoff-live:2011"+AD4-` +
		comment(17<<10) + `+ADw-/MPD+AD4-<!-- <MPD -->`

	bodies := map[string]struct{ ct, body string }{
		"manifest.mpd": {"application/octet-stream", "\x00\x01 not sniffable"},
		"typed":        {"application/dash+xml; charset=utf-8", "\x00\x01 not sniffable"},
		"prolog":       {"application/octet-stream", shortProlog + root},
		"longprolog":   {"application/octet-stream", longProlog + root},
		"lower":        {"application/octet-stream", decl + strings.ToLower(root)},
		"prefixed":     {"application/octet-stream", shortProlog + `<dash:MPD xmlns:dash="urn:mpeg:dash:schema:mpd:2011"/>`},
		"noroot":       {"application/xml", longProlog},
		"pidesync":     {"application/octet-stream", piDesync},
		"deepmention":  {"application/xml", deepMention},
		"utf16mpd":     {"application/octet-stream", utf16MPD},
		"sp600":        {"application/octet-stream", sp(600) + root},
		"bomsp600":     {"application/octet-stream", "\xef\xbb\xbf" + sp(600) + root},
		"sp20000":      {"application/octet-stream", sp(20000) + root},
		"subs":         {"application/xml", xmlOther},
		// Not text/html: local HTML-filtering proxies (seen on Windows dev
		// machines) rewrite those bodies even on loopback.
		"html": {"application/octet-stream", htmlErr},
		"seg":  {"video/mp2t", media},
		// A byte range from the middle of a file that starts with '<'.
		"slice": {"video/mp4", slice},
		"ctrl":  {"application/octet-stream", ctrlLong},
		"utf7":  {"application/xml", utf7Padded},
		"empty": {"video/mp2t", ""},
	}
	mux := http.NewServeMux()
	for name, b := range bodies {
		mux.HandleFunc("/cdn/"+name, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", b.ct)
			_, _ = io.WriteString(w, b.body)
		})
		mux.Handle("/go/"+strings.TrimSuffix(name, ".mpd"), http.RedirectHandler("/cdn/"+name, http.StatusFound))
	}
	upstream := httptest.NewServer(mux)
	defer upstream.Close()

	cases := []struct {
		name, src, wantBody string
		wantStatus          int
	}{
		{"final URL .mpd, unsniffable body", "manifest", "", http.StatusUnsupportedMediaType},
		{"dash+xml content type", "typed", "", http.StatusUnsupportedMediaType},
		{"MPD behind a 2 KiB prolog", "prolog", "", http.StatusUnsupportedMediaType},
		{"MPD behind a 17 KiB comment", "longprolog", "", http.StatusUnsupportedMediaType},
		{"lowercase mpd root", "lower", "", http.StatusUnsupportedMediaType},
		{"prefixed dash:MPD root past 512 bytes", "prefixed", "", http.StatusUnsupportedMediaType},
		{"no root within 16 KiB", "noroot", "", http.StatusUnsupportedMediaType},
		{"control byte, MPD root past 16 KiB", "ctrl", "", http.StatusUnsupportedMediaType},
		{"UTF-7 with spaces around =, padded past 16 KiB", "utf7", "", http.StatusUnsupportedMediaType},
		{"PI in a DOCTYPE internal subset", "pidesync", "", http.StatusUnsupportedMediaType},
		{"<mpd past 512 bytes, not the root", "deepmention", "", http.StatusUnsupportedMediaType},
		{"MPD behind 600 spaces", "sp600", "", http.StatusUnsupportedMediaType},
		{"MPD behind a BOM and 600 spaces", "bomsp600", "", http.StatusUnsupportedMediaType},
		{"MPD behind 20000 spaces", "sp20000", "", http.StatusUnsupportedMediaType},
		{"UTF-16 MPD (not probeable by ffmpeg)", "utf16mpd", utf16MPD, http.StatusOK},
		{"other XML", "subs", xmlOther, http.StatusOK},
		{"HTML error page", "html", htmlErr, http.StatusOK},
		{"media", "seg", media, http.StatusOK},
		{"media slice starting with '<'", "slice", slice, http.StatusOK},
		{"empty body", "empty", "", http.StatusOK},
	}
	r := newMediaRelay(false)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			relayURL, err := r.register(upstream.URL + "/go/" + c.src)
			if err != nil {
				t.Fatalf("register: %v", err)
			}
			resp, err := http.Get(relayURL) //nolint:gosec // our own loopback relay
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			got, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != c.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, c.wantStatus)
			}
			if c.wantStatus == http.StatusOK && string(got) != c.wantBody {
				t.Errorf("body altered: got %d bytes, want %d", len(got), len(c.wantBody))
			}
		})
	}
}

// utf16LE encodes s as UTF-16LE.
func utf16LE(s string) string {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = append(b, byte(u), byte(u>>8))
	}
	return string(b)
}

// TestXMLRootIsMPDOrUnknown covers the prolog walk directly.
func TestXMLRootIsMPDOrUnknown(t *testing.T) {
	for _, c := range []struct {
		name, head string
		want       bool
	}{
		{"MPD", `<?xml version="1.0"?><MPD/>`, true},
		{"mpd with BOM", "\xef\xbb\xbf<mpd/>", true},
		{"namespaced", `<dash:MPD xmlns:dash="urn:mpeg:dash:schema:mpd:2011"/>`, true},
		{"latin-1 declaration", `<?xml version="1.0" encoding="ISO-8859-1"?><!DOCTYPE x><MPD/>`, true},
		{"truncated comment", `<?xml version="1.0"?><!-- never closed`, true},
		{"TTML", `<?xml version="1.0"?><!-- c --><tt/>`, false},
		{"HTML", "<!DOCTYPE html><html><body>MPD</body></html>", false},
		{"latin-1 TTML", `<?xml version="1.0" encoding="ISO-8859-1"?><tt/>`, false},
		{"DOCTYPE internal subset", `<!DOCTYPE tt [<!ENTITY a "b">]><tt/>`, true},
		{"plain DOCTYPE", `<!DOCTYPE tt SYSTEM "tt.dtd"><tt/>`, false},
		{"windows-1252 TTML", `<?xml version="1.0" encoding="windows-1252"?><tt/>`, false},
		// libxml2 decodes this as <!--<x>--> and then an MPD root.
		{"UTF-7 declaration", `<?xml version="1.0" encoding="UTF-7"?>+ADwAIQAtAC0-<x>-->+ADw-MPD+AD4-`, true},
		{"Shift_JIS declaration", `<?xml version="1.0" encoding="Shift_JIS"?><tt/>`, true},
		// Whitespace around '=' hides the encoding from encoding/xml but
		// not from libxml2.
		{"UTF-7, spaces around =", `<?xml version="1.0" encoding = "UTF-7"?>+ADwAIQAtAC0-<x>-->+ADw-MPD+AD4-`, true},
		{"UTF-7, tab and newline around =", "<?xml version=\"1.0\" encoding\t=\n'UTF-7'?><tt/>", true},
		{"utf-7, single quotes", `<?xml version='1.0' encoding='utf-7'?><tt/>`, true},
		{"mixed-case UTF-8, spaces around =", `<?xml version = "1.0" encoding = 'Utf-8' ?><tt/>`, false},
		{"ISO-8859-15, newline around =", "<?xml version=\"1.0\" encoding\n=\n\"ISO-8859-15\"?><tt/>", false},
		{"unparseable declaration", `<?xml version="1.0" encoding=UTF-8?><tt/>`, true},
		{"unknown pseudo-attribute", `<?xml version="1.0" Encoding="UTF-7"?><tt/>`, true},
		{"unclosed declaration", `<?xml version="1.0" encoding="UTF-8"`, true},
		{"xml-stylesheet PI", `<?xml-stylesheet href="a.css"?><tt/>`, false},
	} {
		if got := xmlRootIsMPDOrUnknown([]byte(c.head)); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// TestLooksLikeXML: a head that is empty after the BOM and whitespace is
// undecided and gets the deeper look; an empty body is not XML.
func TestLooksLikeXML(t *testing.T) {
	for _, c := range []struct {
		name, head string
		want       bool
	}{
		{"root", "<MPD/>", true},
		{"BOM and whitespace before root", "\xef\xbb\xbf \r\n\t<tt/>", true},
		{"whitespace only", strings.Repeat(" ", relaySniffBytes), true},
		{"BOM and whitespace only", "\xef\xbb\xbf" + strings.Repeat("\n", relaySniffBytes-3), true},
		{"BOM only", "\xef\xbb\xbf", true},
		{"empty", "", false},
		{"playlist", "#EXTM3U\n", false},
		{"media", "\x47\x40\x00\x10", false},
		{"text before root", "x<MPD/>", false},
	} {
		if got := looksLikeXML([]byte(c.head)); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
