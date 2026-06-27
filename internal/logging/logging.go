// Package logging provides the application's structured logger, built on the
// standard library's log/slog. It offers leveled, component-tagged, key=value
// logging with a compact human-readable text format (the default) or JSON.
//
// Configuration is environment-driven and applied once via Setup:
//
//	STREMIO_LOG_LEVEL   debug | info | warn | error   (default: info)
//	STREMIO_LOG_FORMAT  text  | json                  (default: text)
//
// Subsystems obtain a tagged logger with For("engine"), which stamps every
// record with component=engine so output can be filtered and read at a glance:
//
//	2026-06-27T12:00:37.700Z INFO  engine: disk piece cache  path=/var/cache size=0
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// componentKey is the attribute key the text handler lifts out of the record
// and renders as a "name:" prefix instead of an ordinary key=value field.
const componentKey = "component"

// Setup configures the process-wide slog default logger from the environment.
// It is safe to call once, early in startup, before any logging occurs.
func Setup() {
	slog.SetDefault(slog.New(newHandler(os.Stderr, levelFromEnv(), formatFromEnv())))
}

// For returns a logger tagged with the given component name. The component is
// rendered as a leading "name:" by the text handler and as component=name by
// the JSON handler.
func For(component string) *slog.Logger {
	return slog.Default().With(componentKey, component)
}

// Fatal logs msg at error level with the given key=value args, then exits the
// process with status 1. It is the structured replacement for log.Fatalf.
func Fatal(msg string, args ...any) {
	slog.Default().Error(msg, args...)
	os.Exit(1)
}

func levelFromEnv() slog.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("STREMIO_LOG_LEVEL"))) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

type format int

const (
	formatText format = iota
	formatJSON
)

func formatFromEnv() format {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("STREMIO_LOG_FORMAT")), "json") {
		return formatJSON
	}
	return formatText
}

// newHandler builds the slog.Handler for the requested format and level.
func newHandler(w *os.File, level slog.Level, f format) slog.Handler {
	if f == formatJSON {
		return slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	}
	return newTextHandler(w, level)
}
