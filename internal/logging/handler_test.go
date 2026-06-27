package logging

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTextHandlerFormat(t *testing.T) {
	var buf bytes.Buffer
	h := newTextHandler(&buf, slog.LevelInfo)
	logger := slog.New(h).With(componentKey, "engine")
	logger.Info("disk piece cache", "path", "/var/cache", "bytes", 123)

	out := buf.String()
	for _, want := range []string{"INFO ", "engine: disk piece cache", "path=/var/cache", "bytes=123"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q missing %q", out, want)
		}
	}
}

func TestTextHandlerQuotesValuesWithSpaces(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(newTextHandler(&buf, slog.LevelInfo))
	logger.Info("msg", "file", "a b.mkv")
	if !strings.Contains(buf.String(), `file="a b.mkv"`) {
		t.Errorf("value with space not quoted: %q", buf.String())
	}
}

func TestTextHandlerLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(newTextHandler(&buf, slog.LevelWarn))
	logger.Info("suppressed")
	logger.Warn("kept")
	out := buf.String()
	if strings.Contains(out, "suppressed") {
		t.Errorf("info record should be filtered at warn level: %q", out)
	}
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "kept") {
		t.Errorf("warn record missing: %q", out)
	}
}

func TestTextHandlerTimestampShape(t *testing.T) {
	var buf bytes.Buffer
	h := newTextHandler(&buf, slog.LevelInfo)
	rec := slog.NewRecord(time.Date(2026, 6, 27, 12, 0, 37, int(700*time.Millisecond), time.UTC), slog.LevelInfo, "x", 0)
	if err := h.Handle(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "2026-06-27T12:00:37.700Z ") {
		t.Errorf("unexpected timestamp prefix: %q", buf.String())
	}
}

func TestResponseRecorder(t *testing.T) {
	rr := NewResponseRecorder(httptest.NewRecorder())
	rr.WriteHeader(206)
	n, _ := rr.Write([]byte("hello"))
	if rr.Status != 206 || rr.Bytes != int64(n) || rr.StatusOrOK() != 206 {
		t.Errorf("recorder captured status=%d bytes=%d", rr.Status, rr.Bytes)
	}
	// Flush must be a safe no-op even when the underlying writer is a Flusher.
	rr.Flush()
}
