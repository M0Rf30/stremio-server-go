// Package api — fifth batch: queryBitmagnet and bitmagnet/torznab stream
// handlers exercised through mocked backends via httptest.
package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/M0Rf30/stremio-server-go/internal/types"
)

// graphqlOK builds a Bitmagnet GraphQL response body containing the given items.
func graphqlOK(items string) string {
	return `{"data":{"torrentContent":{"search":{"items":` + items + `}}}}`
}

// graphqlError builds a Bitmagnet GraphQL response with a top-level error.
const graphqlError = `{"data":null,"errors":[{"message":"boom: internal error"}]}`

// ─── queryBitmagnet ──────────────────────────────────────────────────────────

func TestQueryBitmagnet_EmptyResult(t *testing.T) {
	srv := newHTTPTestServer(graphqlOK(`[]`))
	defer srv.Close()
	req := newCtxRequest()
	items, err := queryBitmagnet(req, srv.URL, "test query")
	if err != nil {
		t.Fatalf("queryBitmagnet error: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("got %d items; want 0", len(items))
	}
}

func TestQueryBitmagnet_WithItems(t *testing.T) {
	const oneItem = `[{
		"infoHash": "aabbccddeeff00112233445566778899aabbccdd",
		"contentType": "movie",
		"title": "Test Movie",
		"seeders": 42,
		"videoResolution": "1080p",
		"torrent": {"name": "test.torrent", "size": 1073741824, "filesCount": 1}
	}]`
	srv := newHTTPTestServer(graphqlOK(oneItem))
	defer srv.Close()
	req := newCtxRequest()
	items, err := queryBitmagnet(req, srv.URL, "test query")
	if err != nil {
		t.Fatalf("queryBitmagnet error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items; want 1", len(items))
	}
	if items[0].InfoHash != "aabbccddeeff00112233445566778899aabbccdd" {
		t.Errorf("infoHash = %q", items[0].InfoHash)
	}
	if items[0].Seeders != 42 {
		t.Errorf("seeders = %d; want 42", items[0].Seeders)
	}
}

func TestQueryBitmagnet_GraphQLError(t *testing.T) {
	srv := newHTTPTestServer(graphqlError)
	defer srv.Close()
	req := newCtxRequest()
	_, err := queryBitmagnet(req, srv.URL, "test")
	if err == nil {
		t.Fatal("expected error for GraphQL-level error, got nil")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %v; want it to contain \"boom\"", err)
	}
}

func TestQueryBitmagnet_MalformedJSON(t *testing.T) {
	srv := newHTTPTestServer("{not json")
	defer srv.Close()
	req := newCtxRequest()
	_, err := queryBitmagnet(req, srv.URL, "test")
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

func TestQueryBitmagnet_Non200(t *testing.T) {
	// Server returning 500 — the decode will then fail (empty body).
	srv := newHTTPTestServerStatus(http.StatusInternalServerError, "")
	defer srv.Close()
	req := newCtxRequest()
	_, err := queryBitmagnet(req, srv.URL, "test")
	if err == nil {
		t.Fatal("expected error for HTTP 500, got nil")
	}
}

// ─── bitmagnetStream handler with BitmagnetURL set ────────────────────────────

func TestHandlerBitmagnetStream_WithMockedEndpoint(t *testing.T) {
	const items = `[{
		"infoHash": "aabbccddeeff00112233445566778899aabbccdd",
		"contentType": "movie",
		"title": "Test Movie",
		"seeders": 10,
		"torrent": {"name": "test.torrent", "size": 1073741824}
	}]`
	srv := newHTTPTestServer(graphqlOK(items))
	defer srv.Close()

	// Clear the Cinemeta cache so resolveCinemeta does a fresh (failing) lookup.
	cineMetaMu.Lock()
	for k := range cineMetaCache {
		delete(cineMetaCache, k)
	}
	cineMetaMu.Unlock()

	cfg := buildCfg()
	cfg.BitmagnetURL = srv.URL
	h := New(newFakeEM(), &fakeSS{}, &fakeProber{}, cfg)

	rec := serve(t, h, http.MethodGet, "/bitmagnet/stream/movie/tt1234567.json", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	m := decodeJSON(t, rec.Body.Bytes())
	streams, ok := m["streams"].([]any)
	if !ok {
		t.Fatalf("streams missing/not array; got %v", m)
	}
	if len(streams) != 1 {
		t.Errorf("streams = %d items; want 1 (the mocked Bitmagnet item)", len(streams))
	}
}

func TestHandlerBitmagnetStream_InvalidType(t *testing.T) {
	cfg := buildCfg()
	cfg.BitmagnetURL = "http://example.invalid/" // set but type will be rejected first
	h := New(newFakeEM(), &fakeSS{}, &fakeProber{}, cfg)
	rec := serve(t, h, http.MethodGet, "/bitmagnet/stream/other/tt123.json", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	m := decodeJSON(t, rec.Body.Bytes())
	streams, _ := m["streams"].([]any)
	if len(streams) != 0 {
		t.Errorf("streams = %v; want [] (invalid type)", streams)
	}
}

// ─── torznabStream handler with TorznabURL set ────────────────────────────────

func TestHandlerTorznabStream_WithMockedEndpoint(t *testing.T) {
	srv := newHTTPTestServer(torznabXML)
	defer srv.Close()

	cfg := buildCfg()
	cfg.TorznabURL = srv.URL
	h := New(newFakeEM(), &fakeSS{}, &fakeProber{}, cfg)

	// Clear the Cinemeta cache so resolveCinemeta doesn't short-circuit.
	cineMetaMu.Lock()
	for k := range cineMetaCache {
		delete(cineMetaCache, k)
	}
	cineMetaMu.Unlock()

	rec := serve(t, h, http.MethodGet, "/torznab/stream/movie/tt1234567.json", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	m := decodeJSON(t, rec.Body.Bytes())
	streams, ok := m["streams"].([]any)
	if !ok {
		t.Fatalf("streams missing/not array; got %v", m)
	}
	if len(streams) != 1 {
		t.Errorf("streams = %d items; want 1 (the mocked Torznab result)", len(streams))
	}
}

// ─── buildCfg helper (a types.Config with sensible defaults for tests) ────────

func buildCfg() types.Config {
	return types.Config{
		HTTPPort: 11470,
		WebUI:    "https://web.stremio.com/",
	}
}
