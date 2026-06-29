// Package api — third batch of tests covering manifest routes, addon handlers,
// YouTube handler, certprovision helpers, torznab helpers, and more.
package api

import (
	"archive/zip"
	"bytes"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/M0Rf30/stremio-server-go/internal/types"
)

// ─── handler with localAddonEnabled=true ─────────────────────────────────────

func localAddonHandler(t *testing.T) http.Handler {
	t.Helper()
	ss := &fakeSS{vals: map[string]any{"localAddonEnabled": true}}
	cfg := types.Config{HTTPPort: 11470, WebUI: "https://web.stremio.com/"}
	return New(newFakeEM(), ss, &fakeProber{}, cfg)
}

// ─────────────────────────────────────────────────────────────────────────────
// Manifest and addon routes
// ─────────────────────────────────────────────────────────────────────────────

// ─── /bitmagnet ───────────────────────────────────────────────────────────────

func TestHandlerBitmagnetManifest(t *testing.T) {
	h := newHandler(t)
	rec := serve(t, h, http.MethodGet, "/bitmagnet/manifest.json", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	m := decodeJSON(t, rec.Body.Bytes())
	if id, _ := m["id"].(string); id != "community.stremioservergo.bitmagnet" {
		t.Errorf("bitmagnet manifest id = %q; want community.stremioservergo.bitmagnet", id)
	}
}

func TestHandlerBitmagnetUnknown404(t *testing.T) {
	h := newHandler(t)
	for _, path := range []string{"/bitmagnet/unknown", "/bitmagnet"} {
		rec := serve(t, h, http.MethodGet, path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s status = %d; want 404", path, rec.Code)
		}
	}
}

// ─── /torznab ─────────────────────────────────────────────────────────────────

func TestHandlerTorznabManifest(t *testing.T) {
	h := newHandler(t)
	rec := serve(t, h, http.MethodGet, "/torznab/manifest.json", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	m := decodeJSON(t, rec.Body.Bytes())
	if id, _ := m["id"].(string); id != "community.stremioservergo.torznab" {
		t.Errorf("torznab manifest id = %q; want community.stremioservergo.torznab", id)
	}
}

func TestHandlerTorznabUnknown404(t *testing.T) {
	h := newHandler(t)
	rec := serve(t, h, http.MethodGet, "/torznab/unknown", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

// ─── /local-addon ─────────────────────────────────────────────────────────────

func TestHandlerLocalAddonManifest(t *testing.T) {
	h := localAddonHandler(t)
	rec := serve(t, h, http.MethodGet, "/local-addon/manifest.json", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	m := decodeJSON(t, rec.Body.Bytes())
	if id, _ := m["id"].(string); id != "org.stremio.local.go" {
		t.Errorf("local addon manifest id = %q; want org.stremio.local.go", id)
	}
}

func TestHandlerLocalAddonCatalog_Empty(t *testing.T) {
	h := localAddonHandler(t)
	rec := serve(t, h, http.MethodGet, "/local-addon/catalog/movie/top.json", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	m := decodeJSON(t, rec.Body.Bytes())
	if _, ok := m["metas"].([]any); !ok {
		t.Errorf("catalog missing metas array; got %v", m)
	}
}

func TestHandlerLocalAddonMeta_Unknown(t *testing.T) {
	h := localAddonHandler(t)
	rec := serve(t, h, http.MethodGet, "/local-addon/meta/movie/local:abc123.json", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	m := decodeJSON(t, rec.Body.Bytes())
	if _, ok := m["meta"]; !ok {
		t.Errorf("local addon meta missing meta key; got %v", m)
	}
}

func TestHandlerLocalAddonStream_Unknown(t *testing.T) {
	h := localAddonHandler(t)
	rec := serve(t, h, http.MethodGet, "/local-addon/stream/movie/local:abc123.json", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	m := decodeJSON(t, rec.Body.Bytes())
	if _, ok := m["streams"]; !ok {
		t.Errorf("local addon stream missing streams key; got %v", m)
	}
}

func TestHandlerLocalAddonDisabled_Returns404(t *testing.T) {
	h := newHandler(t) // localAddonEnabled not set → false
	rec := serve(t, h, http.MethodGet, "/local-addon/manifest.json", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 (addon disabled)", rec.Code)
	}
}

// ─── YouTube handler ──────────────────────────────────────────────────────────

func TestHandlerYT_InvalidID(t *testing.T) {
	h := newHandler(t)
	// '@' not in ytIDRe → 400
	rec := serve(t, h, http.MethodGet, "/yt/invalid@id", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("GET /yt/invalid@id status = %d; want 400", rec.Code)
	}
}

func TestHandlerYT_ValidIDNoYtdlp(t *testing.T) {
	if _, err := exec.LookPath("yt-dlp"); err == nil {
		t.Skip("yt-dlp found on PATH; skipping to avoid real network call")
	}
	h := newHandler(t)
	rec := serve(t, h, http.MethodGet, "/yt/dQw4w9WgXcQ", nil)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d; want 501 (yt-dlp not found)", rec.Code)
	}
}

// ─── NZB create (with httptest NZB server) ────────────────────────────────────

const minimalNZBXML = `<?xml version="1.0" encoding="UTF-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file subject="&quot;video.mkv&quot; yEnc" poster="test@test.com">
    <segments>
      <segment bytes="1024" number="1">abc123@usenet.example.com</segment>
    </segments>
  </file>
</nzb>`

func TestHandlerNZBCreate_Success(t *testing.T) {
	nzbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-nzb")
		_, _ = w.Write([]byte(minimalNZBXML))
	}))
	defer nzbSrv.Close()

	h := newHandler(t)
	body := fmt.Sprintf(`{"servers":["nntp://news.example.com"],"nzbUrl":"%s/test.nzb"}`, nzbSrv.URL)
	req := httptest.NewRequest(http.MethodPost, "/nzb/create", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /nzb/create status = %d; body = %s", rec.Code, rec.Body.String())
	}
	m := decodeJSON(t, rec.Body.Bytes())
	if _, ok := m["key"].(string); !ok {
		t.Errorf("nzb create response missing key: %v", m)
	}
}

// ─── Archive create (zip) with httptest server ────────────────────────────────

func buildTestZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	fw, err := zw.Create("video.mkv")
	if err != nil {
		t.Fatalf("zip.Create: %v", err)
	}
	_, _ = fw.Write([]byte("fake video content for testing purposes"))
	if err := zw.Close(); err != nil {
		t.Fatalf("zip.Close: %v", err)
	}
	return buf.Bytes()
}

func TestHandlerArchiveCreate_ZipSuccess(t *testing.T) {
	zipData := buildTestZip(t)
	zipSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(zipData)
	}))
	defer zipSrv.Close()

	h := newHandler(t)
	body := fmt.Sprintf(`{"url":"%s/test.zip"}`, zipSrv.URL)
	req := httptest.NewRequest(http.MethodPost, "/zip/create", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /zip/create status = %d; body = %s", rec.Code, rec.Body.String())
	}
	m := decodeJSON(t, rec.Body.Bytes())
	if _, ok := m["key"].(string); !ok {
		t.Errorf("archive create missing key: %v", m)
	}
}

// ─── FTP DLNA headers ────────────────────────────────────────────────────────

func TestFtpDLNAHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	ftpDLNAHeaders(rec.Header())
	if v := rec.Header().Get("transferMode.dlna.org"); v != "Streaming" {
		t.Errorf("transferMode.dlna.org = %q; want Streaming", v)
	}
	if v := rec.Header().Get("contentFeatures.dlna.org"); !strings.Contains(v, "DLNA.ORG_OP=01") {
		t.Errorf("contentFeatures.dlna.org = %q; want DLNA.ORG_OP=01", v)
	}
}

// ─── SetCertReloadHook ────────────────────────────────────────────────────────

func TestSetCertReloadHook(t *testing.T) {
	s := &server{}
	called := false
	s.SetCertReloadHook(func() { called = true })
	if s.certReload == nil {
		t.Fatal("certReload should be non-nil after SetCertReloadHook")
	}
	s.certReload()
	if !called {
		t.Error("hook was not called")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Torznab pure helper unit tests
// ─────────────────────────────────────────────────────────────────────────────

func makeTestItem(attrs map[string]string, encURL, encLen string) tnItem {
	item := tnItem{Enclosure: tnEnclosure{URL: encURL, Length: encLen}}
	for k, v := range attrs {
		item.Attrs = append(item.Attrs, tnAttr{Name: k, Value: v})
	}
	return item
}

func TestTnAttrVal(t *testing.T) {
	item := makeTestItem(map[string]string{"seeders": "42", "size": "1000"}, "", "")
	if got := tnAttrVal(item, "seeders"); got != "42" {
		t.Errorf("tnAttrVal(seeders) = %q; want 42", got)
	}
	if got := tnAttrVal(item, "missing"); got != "" {
		t.Errorf("tnAttrVal(missing) = %q; want empty", got)
	}
}

func TestTnNormalizeHash(t *testing.T) {
	validHex := "aabbccddeeff00112233445566778899aabbccdd"
	// 40-char valid hex
	if got := tnNormalizeHash(validHex); got != validHex {
		t.Errorf("tnNormalizeHash(hex) = %q; want %q", got, validHex)
	}
	// 40-char uppercase hex
	if got := tnNormalizeHash(strings.ToUpper(validHex)); got != validHex {
		t.Errorf("tnNormalizeHash(UPPER) = %q; want %q", got, validHex)
	}
	// Invalid 40-char (non-hex)
	if got := tnNormalizeHash("zzzzccddeeff00112233445566778899aabbccdd"); got != "" {
		t.Errorf("tnNormalizeHash(invalid) = %q; want empty", got)
	}
	// 32-char base32
	raw32 := base32.StdEncoding.EncodeToString([]byte("12345678901234567890"))
	if got := tnNormalizeHash(raw32); got == "" {
		t.Errorf("tnNormalizeHash(base32) should decode to non-empty hex, got empty")
	}
	// Too short → ""
	if got := tnNormalizeHash("abc"); got != "" {
		t.Errorf("tnNormalizeHash(short) = %q; want empty", got)
	}
}

func TestTnSeeders(t *testing.T) {
	t.Run("with seeders attr", func(t *testing.T) {
		item := makeTestItem(map[string]string{"seeders": "15"}, "", "")
		if got := tnSeeders(item); got != 15 {
			t.Errorf("tnSeeders = %d; want 15", got)
		}
	})
	t.Run("without seeders attr → 0", func(t *testing.T) {
		item := makeTestItem(nil, "", "")
		if got := tnSeeders(item); got != 0 {
			t.Errorf("tnSeeders (no attr) = %d; want 0", got)
		}
	})
}

func TestTnItemSize(t *testing.T) {
	t.Run("from size attr", func(t *testing.T) {
		item := makeTestItem(map[string]string{"size": "1073741824"}, "", "")
		if got := tnItemSize(item); got != 1073741824 {
			t.Errorf("tnItemSize = %d; want 1073741824", got)
		}
	})
	t.Run("from enclosure length", func(t *testing.T) {
		item := makeTestItem(nil, "magnet:?xt=urn:btih:abc", "524288000")
		if got := tnItemSize(item); got != 524288000 {
			t.Errorf("tnItemSize = %d; want 524288000", got)
		}
	})
	t.Run("from magnet xl", func(t *testing.T) {
		item := makeTestItem(nil, "magnet:?xt=urn:btih:abc&xl=2147483648", "")
		if got := tnItemSize(item); got != 2147483648 {
			t.Errorf("tnItemSize = %d; want 2147483648", got)
		}
	})
	t.Run("no size → 0", func(t *testing.T) {
		item := makeTestItem(nil, "", "")
		if got := tnItemSize(item); got != 0 {
			t.Errorf("tnItemSize (none) = %d; want 0", got)
		}
	})
}

func TestTnResolveInfoHash(t *testing.T) {
	validHex := "aabbccddeeff00112233445566778899aabbccdd"
	t.Run("from infohash attr", func(t *testing.T) {
		item := makeTestItem(map[string]string{"infohash": validHex}, "", "")
		if got := tnResolveInfoHash(item); got != validHex {
			t.Errorf("tnResolveInfoHash(infohash attr) = %q; want %q", got, validHex)
		}
	})
	t.Run("from enclosure URL magnet", func(t *testing.T) {
		encURL := fmt.Sprintf("magnet:?xt=urn:btih:%s&dn=test", validHex)
		item := makeTestItem(nil, encURL, "")
		if got := tnResolveInfoHash(item); got != validHex {
			t.Errorf("tnResolveInfoHash(enclosure URL) = %q; want %q", got, validHex)
		}
	})
	t.Run("no hash → empty", func(t *testing.T) {
		item := makeTestItem(nil, "http://example.com/torrent", "")
		if got := tnResolveInfoHash(item); got != "" {
			t.Errorf("tnResolveInfoHash (no hash) = %q; want empty", got)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// certprovision helper tests
// ─────────────────────────────────────────────────────────────────────────────

func TestPerrAndError(t *testing.T) {
	e := perr(http.StatusBadGateway, "test error %s", "message")
	if e.status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", e.status)
	}
	if e.Error() != "test error message" {
		t.Errorf("Error() = %q; want \"test error message\"", e.Error())
	}
}

func TestParseCertResult_LegacyFlat(t *testing.T) {
	result := json.RawMessage(`{
		"certificate": "-----BEGIN CERTIFICATE-----\nMIItest\n-----END CERTIFICATE-----",
		"privateKey": "-----BEGIN PRIVATE KEY-----\ntest\n-----END PRIVATE KEY-----",
		"commonName": "*.abc.stremio.rocks"
	}`)
	certPEM, keyPEM, cn, err := parseCertResult(result)
	if err != nil {
		t.Fatalf("parseCertResult error: %v", err)
	}
	if !strings.HasPrefix(certPEM, "-----BEGIN") {
		t.Errorf("certPEM = %q; want PEM format", certPEM)
	}
	if !strings.HasPrefix(keyPEM, "-----BEGIN") {
		t.Errorf("keyPEM = %q; want PEM format", keyPEM)
	}
	if cn != "*.abc.stremio.rocks" {
		t.Errorf("commonName = %q; want *.abc.stremio.rocks", cn)
	}
}

func TestParseCertResult_InvalidJSON(t *testing.T) {
	_, _, _, err := parseCertResult(json.RawMessage(`{invalid`))
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestParseCertResult_DoubleEncoded(t *testing.T) {
	inner := `{"certificate":"-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----","privateKey":"-----BEGIN PRIVATE KEY-----\ntest\n-----END PRIVATE KEY-----","commonName":"*.test.stremio.rocks"}`
	outer, _ := json.Marshal(inner)
	_, _, cn, err := parseCertResult(json.RawMessage(outer))
	if err != nil {
		t.Fatalf("parseCertResult double-encoded error: %v", err)
	}
	if cn != "*.test.stremio.rocks" {
		t.Errorf("commonName = %q; want *.test.stremio.rocks", cn)
	}
}

func TestInstallCertFiles(t *testing.T) {
	dir := t.TempDir()
	certPEM := "-----BEGIN CERTIFICATE-----\nhello\n-----END CERTIFICATE-----"
	keyPEM := "-----BEGIN PRIVATE KEY-----\nworld\n-----END PRIVATE KEY-----"

	if err := installCertFiles(dir, certPEM, keyPEM); err != nil {
		t.Fatalf("installCertFiles error: %v", err)
	}
	certGot, err := os.ReadFile(dir + "/https-cert.pem")
	if err != nil {
		t.Fatalf("ReadFile cert: %v", err)
	}
	if string(certGot) != certPEM {
		t.Errorf("cert content = %q; want %q", certGot, certPEM)
	}
	keyGot, err := os.ReadFile(dir + "/https-key.pem")
	if err != nil {
		t.Fatalf("ReadFile key: %v", err)
	}
	if string(keyGot) != keyPEM {
		t.Errorf("key content = %q; want %q", keyGot, keyPEM)
	}
}

func TestFirstLeaf_NoPEM(t *testing.T) {
	if got := firstLeaf(""); got != nil {
		t.Errorf("firstLeaf('') = %v; want nil", got)
	}
	if got := firstLeaf("-----BEGIN PRIVATE KEY-----\nhello\n-----END PRIVATE KEY-----"); got != nil {
		t.Errorf("firstLeaf(non-cert PEM) = %v; want nil", got)
	}
}

func TestFirstLeaf_InvalidCert(t *testing.T) {
	// CERTIFICATE block with invalid DER → nil
	fakePEM := "-----BEGIN CERTIFICATE-----\nYWJj\n-----END CERTIFICATE-----"
	if got := firstLeaf(fakePEM); got != nil {
		t.Errorf("firstLeaf(invalid cert) = %v; want nil", got)
	}
}

func TestProvisionErrorIs(t *testing.T) {
	e := perr(http.StatusBadRequest, "test %d", 42)
	var pe *provisionError
	if !errors.As(e, &pe) {
		t.Error("expected *provisionError from errors.As")
	}
	if pe.status != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", pe.status)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// castingParams + castingCommand
// ─────────────────────────────────────────────────────────────────────────────

func TestCastingParams_GET(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/casting/dev/player?command=play&source=http://x", nil)
	params := castingParams(req)
	if params.Get("command") != "play" {
		t.Errorf("command = %q; want play", params.Get("command"))
	}
}

func TestCastingParams_POST(t *testing.T) {
	body := strings.NewReader("command=seek&time=30")
	req := httptest.NewRequest(http.MethodPost, "/casting/dev/player", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	params := castingParams(req)
	if params.Get("command") != "seek" {
		t.Errorf("command = %q; want seek", params.Get("command"))
	}
}

func TestCastingParams_JSONBody(t *testing.T) {
	body := strings.NewReader(`{"source":"http://x/stream","time":120}`)
	req := httptest.NewRequest(http.MethodPost, "/casting/dev/player", body)
	req.Header.Set("Content-Type", "application/json")
	params := castingParams(req)
	if got := params.Get("source"); got != "http://x/stream" {
		t.Errorf("source = %q; want http://x/stream", got)
	}
	if got := params.Get("time"); got != "120" {
		t.Errorf("time = %q; want 120 (u64 preserved verbatim)", got)
	}
	if cmd := castingCommand([]string{"casting", "dev", "player"}, params); cmd != "load" {
		t.Errorf("castingCommand = %q; want load (inferred from source)", cmd)
	}
}

func TestCastingCommand(t *testing.T) {
	tests := []struct {
		name   string
		seg    []string
		params url.Values
		want   string
	}{
		{"from path suffix", []string{"casting", "id", "player", "play"}, url.Values{}, "play"},
		{"from command param", []string{"casting", "id", "player"}, url.Values{"command": {"PAUSE"}}, "pause"},
		{"infer from source", []string{"casting", "id", "player"}, url.Values{"source": {"http://x"}}, "load"},
		{"infer from stop", []string{"casting", "id", "player"}, url.Values{"stop": {"1"}}, "stop"},
		{"infer from paused", []string{"casting", "id", "player"}, url.Values{"paused": {"true"}}, "pause"},
		{"infer from time", []string{"casting", "id", "player"}, url.Values{"time": {"120"}}, "seek"},
		{"default → status", []string{"casting", "id", "player"}, url.Values{}, "status"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := castingCommand(tc.seg, tc.params)
			if got != tc.want {
				t.Errorf("castingCommand = %q; want %q", got, tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// localaddon pure helper tests
// ─────────────────────────────────────────────────────────────────────────────

func TestParseFilenameToMeta(t *testing.T) {
	tests := []struct {
		stem      string
		wantName  string
		wantCtype string
		wantYear  int
	}{
		{"The Dark Knight 2008 1080p BluRay", "The Dark Knight", "movie", 2008},
		{"Breaking Bad S01E05 720p", "Breaking Bad", "series", 0},
		{"random file with no info", "", "other", 0},
		{"Inception.2010.1080p", "Inception", "movie", 2010},
	}
	for _, tc := range tests {
		t.Run(tc.stem, func(t *testing.T) {
			got := parseFilenameToMeta(tc.stem)
			if got.name == "" {
				t.Errorf("name = empty; want non-empty for %q", tc.stem)
			}
			if tc.wantName != "" && got.name != tc.wantName {
				t.Errorf("name = %q; want %q", got.name, tc.wantName)
			}
			if tc.wantCtype != "" && got.ctype != tc.wantCtype {
				t.Errorf("ctype = %q; want %q", got.ctype, tc.wantCtype)
			}
			if tc.wantYear != 0 && got.year != tc.wantYear {
				t.Errorf("year = %d; want %d", got.year, tc.wantYear)
			}
		})
	}
}

func TestLocalID_Deterministic(t *testing.T) {
	h1 := localID("/path/to/video.mkv")
	h2 := localID("/path/to/video.mkv")
	if h1 != h2 {
		t.Error("localID is not deterministic")
	}
	h3 := localID("/path/to/other.mkv")
	if h1 == h3 {
		t.Error("localID collision for different paths")
	}
	if len(h1) != 16 {
		t.Errorf("localID length = %d; want 16", len(h1))
	}
}

func TestFileURL(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"/home/user/video.mkv", "file:///home/user/video.mkv"},
		{"/path/with spaces/video.mkv", "file:///path/with%20spaces/video.mkv"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got := fileURL(tc.in)
			if got != tc.want {
				t.Errorf("fileURL(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Archive stream with unknown session → 404
// ─────────────────────────────────────────────────────────────────────────────

func TestHandlerArchiveStream_UnknownSession404(t *testing.T) {
	h := newHandler(t)
	rec := serve(t, h, http.MethodGet, "/zip/stream/nosuchsession/video.mkv", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 for unknown session", rec.Code)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Additional handler error paths
// ─────────────────────────────────────────────────────────────────────────────

func TestHandlerBitmagnetStreamTooFewSegs404(t *testing.T) {
	h := newHandler(t)
	rec := serve(t, h, http.MethodGet, "/bitmagnet/stream/movie", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

func TestHandlerTorznabStreamTooFewSegs404(t *testing.T) {
	h := newHandler(t)
	rec := serve(t, h, http.MethodGet, "/torznab/stream/movie", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rec.Code)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// decodePEMField edge cases
// ─────────────────────────────────────────────────────────────────────────────

func TestDecodePEMField_WhitespaceOnly(t *testing.T) {
	if got := decodePEMField("   "); got != "" {
		t.Errorf("decodePEMField(whitespace) = %q; want empty", got)
	}
}
