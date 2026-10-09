// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package streamproxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func fakeGet(pages map[string]string) pageFetcher {
	return func(_ *http.Request, rawurl string, _ map[string]string) (string, error) {
		if b, ok := pages[rawurl]; ok {
			return b, nil
		}
		return "", errExtract
	}
}

func TestExtractVixCloud(t *testing.T) {
	page := "https://player.example/movie/123"
	html := `<script>window.video = {id: 1};
	window.masterPlaylist = { params: { 'token': 'abcDEF123', 'expires': '1760000000', },
	url: 'https://player.example/playlist/999?b=1', }
	window.canPlayFHD = true</script>`
	r := httptest.NewRequest("GET", "/", nil)
	res, err := extractVixCloud(r, page, fakeGet(map[string]string{
		"https://player.example/api/movie/123":     `{"src":"/embed/777?lang=it"}`,
		"https://player.example/embed/777?lang=it": html,
	}))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(res.URL)
	q := u.Query()
	if u.Host != "player.example" || u.Path != "/playlist/999" || q.Get("token") != "abcDEF123" ||
		q.Get("expires") != "1760000000" || q.Get("h") != "1" || q.Get("b") != "1" {
		t.Errorf("bad url %s", res.URL)
	}
	if res.Endpoint != "/proxy/hls/manifest.m3u8" || res.Headers["Referer"] != "https://player.example/" {
		t.Errorf("bad result %+v", res)
	}
	if _, err := extractVixCloud(r, page, fakeGet(map[string]string{page: "<html></html>"})); err == nil {
		t.Error("expected error for page without playlist")
	}
}

func TestHandleExtractorErrors(t *testing.T) {
	h := New(Config{Password: "pw"})
	for _, tc := range []struct {
		path string
		seg  []string
		code int
	}{
		{"/extractor/video?host=VixCloud&d=https://x.example/e/1", []string{"extractor", "video"}, http.StatusUnauthorized},
		{"/extractor/nope?api_password=pw", []string{"extractor", "nope"}, http.StatusNotFound},
		{"/extractor/video.m3u8?host=Unknown&d=https://x.example/&api_password=pw", []string{"extractor", "video.m3u8"}, http.StatusBadRequest},
		{"/extractor/video?host=vixcloud&d=ftp://x.example/&api_password=pw", []string{"extractor", "video"}, http.StatusBadRequest},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", tc.path, nil)
		r.RemoteAddr = "203.0.113.1:1234"
		h.HandleExtractor(w, r, tc.seg)
		if w.Code != tc.code {
			t.Errorf("%s: code %d want %d (%s)", tc.path, w.Code, tc.code, w.Body.String())
		}
		if tc.code == http.StatusBadRequest {
			var m map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil || m["detail"] == "" {
				t.Errorf("%s: bad error body %s", tc.path, w.Body.String())
			}
		}
	}
}
