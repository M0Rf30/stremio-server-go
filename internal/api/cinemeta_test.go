package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func clearCineMetaCache() {
	cineMetaMu.Lock()
	for k := range cineMetaCache {
		delete(cineMetaCache, k)
	}
	cineMetaMu.Unlock()
}

// TestResolveCinemetaDisabled verifies that an empty base URL disables
// resolution entirely and performs no network request.
func TestResolveCinemetaDisabled(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	}))
	defer srv.Close()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	name, year := resolveCinemeta(req, "", "movie", "tt0468569")
	if name != "" || year != 0 {
		t.Errorf("disabled resolveCinemeta = %q/%d; want \"\"/0", name, year)
	}
	if called {
		t.Error("disabled resolveCinemeta must not make a network request")
	}
}

// TestResolveCinemetaFromMetaAddon verifies that any Cinemeta-compatible meta
// addon base URL is queried at /meta/{type}/{id}.json and parsed.
func TestResolveCinemetaFromMetaAddon(t *testing.T) {
	clearCineMetaCache()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/meta/movie/tt0468569.json" {
			t.Errorf("unexpected meta path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"meta":{"name":"The Dark Knight","year":2008}}`))
	}))
	defer srv.Close()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	name, year := resolveCinemeta(req, srv.URL, "movie", "tt0468569")
	if name != "The Dark Knight" || year != 2008 {
		t.Errorf("resolveCinemeta = %q/%d; want The Dark Knight/2008", name, year)
	}
}

// TestResolveCinemetaTrailingSlash verifies a base URL with a trailing slash
// still builds a valid /meta path (no double slash).
func TestResolveCinemetaTrailingSlash(t *testing.T) {
	clearCineMetaCache()
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"meta":{"name":"Foo","releaseInfo":"2019"}}`))
	}))
	defer srv.Close()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	name, year := resolveCinemeta(req, srv.URL+"/", "series", "tt1234567")
	if gotPath != "/meta/series/tt1234567.json" {
		t.Errorf("path = %q; want /meta/series/tt1234567.json", gotPath)
	}
	if name != "Foo" || year != 2019 {
		t.Errorf("resolveCinemeta = %q/%d; want Foo/2019", name, year)
	}
}
