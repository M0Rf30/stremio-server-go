package logging

import (
	"io"
	"log/slog"
	"testing"
)

func BenchmarkTextHandlerInfo(b *testing.B) {
	logger := slog.New(newTextHandler(io.Discard, slog.LevelInfo)).With(componentKey, "engine")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		logger.Info("disk piece cache", "path", "/var/cache", "bytes", 123, "info_hash", "deadbeefcafe")
	}
}

func BenchmarkFor(b *testing.B) {
	slog.SetDefault(slog.New(newTextHandler(io.Discard, slog.LevelInfo)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = For("engine")
	}
}

// BenchmarkForAndInfo models a full log call site (For + one record), the
// pattern used by engine/proxy/addon log lines.
func BenchmarkForAndInfo(b *testing.B) {
	slog.SetDefault(slog.New(newTextHandler(io.Discard, slog.LevelInfo)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		For("bitmagnet").Warn("cinemeta lookup failed", "type", "movie", "imdb", "tt0468569")
	}
}
