package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/M0Rf30/stremio-server-go/internal/types"
)

// discardRW is a minimal http.ResponseWriter that discards output, so
// benchmarks measure handler work rather than httptest.Recorder buffering.
type discardRW struct{ h http.Header }

func (d *discardRW) Header() http.Header {
	if d.h == nil {
		d.h = http.Header{}
	}
	return d.h
}
func (d *discardRW) Write(p []byte) (int, error) { return len(p), nil }
func (d *discardRW) WriteHeader(int)             {}

func benchHandler(engines ...*fakeEngine) http.Handler {
	cfg := types.Config{HTTPPort: 11470, WebUI: "https://web.stremio.com/"}
	return New(newFakeEM(engines...), &fakeSS{}, &fakeProber{}, cfg)
}

func benchServe(b *testing.B, h http.Handler, method, target string, hdr http.Header) {
	b.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		req.Header[k] = v
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(&discardRW{}, req)
	}
}

func BenchmarkHeartbeat(b *testing.B) { benchServe(b, benchHandler(), "GET", "/heartbeat", nil) }
func BenchmarkSettingsGet(b *testing.B) {
	benchServe(b, benchHandler(), "GET", "/settings", nil)
}
func BenchmarkNetworkInfo(b *testing.B) {
	benchServe(b, benchHandler(), "GET", "/network-info", nil)
}
func BenchmarkList(b *testing.B) {
	benchServe(b, benchHandler(testEngine()), "GET", "/list", nil)
}
func BenchmarkStatsJSON(b *testing.B) {
	benchServe(b, benchHandler(testEngine()), "GET", "/"+testIH+"/stats.json", nil)
}
func BenchmarkStreamRange(b *testing.B) {
	benchServe(b, benchHandler(testEngine()), "GET", "/"+testIH+"/0", http.Header{"Range": {"bytes=0-15"}})
}
func BenchmarkStreamFull(b *testing.B) {
	benchServe(b, benchHandler(testEngine()), "GET", "/"+testIH+"/0", nil)
}
