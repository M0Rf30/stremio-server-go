// Package app hosts the reusable server bootstrap (env parsing, subsystem
// wiring, HTTP/HTTPS listeners, graceful shutdown) shared by the
// cmd/stremio-server executable and the cmd/libstremio c-shared library.
//
// Configuration is resolved through a Lookup function rather than direct
// os.Getenv calls so the exact same code path can be driven either by the
// real process environment (OSLookup, used by the executable) or by an
// in-memory map decoded from JSON (MapLookup, used by library mode's
// ServerStart envJSON parameter).
package app

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/M0Rf30/stremio-server-go/internal/logging"
)

// Lookup resolves a configuration key, mirroring os.LookupEnv's (value, ok)
// contract so unset and explicitly-empty values remain distinguishable.
type Lookup func(key string) (string, bool)

// OSLookup adapts the process environment to Lookup. It is the default used
// when Config.Lookup is nil, preserving the historical os.Getenv-based
// behavior of the stremio-server executable.
func OSLookup(key string) (string, bool) { return os.LookupEnv(key) }

// MapLookup adapts a plain string map (e.g. library mode's envJSON, decoded
// once at ServerStart) to Lookup.
func MapLookup(m map[string]string) Lookup {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

// getenv returns lookup(key) if present and non-empty, else def.
func getenv(lookup Lookup, key, def string) string {
	if v, ok := lookup(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(lookup Lookup, key string, def int) int {
	if v, ok := lookup(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		logging.For("config").Warn("invalid integer env", "key", key, "value", v, "default", def)
	}
	return def
}

func envInt64(lookup Lookup, key string, def int64) int64 {
	if v, ok := lookup(key); ok && v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
		logging.For("config").Warn("invalid integer env", "key", key, "value", v, "default", def)
	}
	return def
}

// envFloat parses a non-negative float env var. Unset, unparseable, or negative
// → def. Used for ratio-style knobs where a negative value is meaningless.
func envFloat(lookup Lookup, key string, def float64) float64 {
	if v, ok := lookup(key); ok && v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return f
		}
		logging.For("config").Warn("invalid float env", "key", key, "value", v, "default", def)
	}
	return def
}

// envBool parses a boolean env var. Unset → def. "0", "false", "no", "off"
// (case-insensitive) → false; any other non-empty value → true.
func envBool(lookup Lookup, key string, def bool) bool {
	v, _ := lookup(key)
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// envDuration parses a duration env var. Accepts either a Go duration string
// (time.ParseDuration, e.g. "90s", "5m30s") or a bare non-negative integer
// interpreted as whole seconds — matching every historical *_TIMEOUT-style
// knob in this file (STREMIO_TORRENT_IDLE_TIMEOUT, etc.), which were plain
// seconds long before any duration-string knob existed. Unset/empty → def.
// Unparseable or negative → def, with a warning logged.
func envDuration(lookup Lookup, key string, def time.Duration) time.Duration {
	v, ok := lookup(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	v = strings.TrimSpace(v)
	if d, err := time.ParseDuration(v); err == nil {
		if d < 0 {
			logging.For("config").Warn("negative duration env", "key", key, "value", v, "default", def)
			return def
		}
		return d
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n < 0 {
			logging.For("config").Warn("negative duration env", "key", key, "value", v, "default", def)
			return def
		}
		return time.Duration(n) * time.Second
	}
	logging.For("config").Warn("invalid duration env", "key", key, "value", v, "default", def)
	return def
}

// ffmpegBitrateRe matches the bitrate syntax ffmpeg's -b:v/-maxrate/-bufsize
// options accept: an integer or decimal number with an optional k/K/m/M/g/G
// (SI, decimal) suffix, e.g. "8M", "800k", "8000000".
var ffmpegBitrateRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[kKmMgG]?$`)

// envBitrate parses an ffmpeg-style bitrate env var (see ffmpegBitrateRe).
// Unset/empty → def. A value that doesn't match ffmpeg's own bitrate syntax
// is rejected (with a warning) and def is kept, since it would otherwise
// only fail much later as an opaque ffmpeg exit-code-1 on the first segment
// transcode.
func envBitrate(lookup Lookup, key, def string) string {
	v, ok := lookup(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	v = strings.TrimSpace(v)
	if !ffmpegBitrateRe.MatchString(v) {
		logging.For("config").Warn("invalid bitrate env", "key", key, "value", v, "default", def)
		return def
	}
	return v
}

// allowedOrigins parses STREMIO_ALLOWED_ORIGINS (Contract 2 / SEC-1): a
// comma-separated list of extra origins the HTTP API accepts beyond the
// built-in Stremio-web/localhost/own-IP allowlist, in addition to the
// "scheme://host[:port]" / "host[:port]" / "*.domain" wildcard forms. A
// value of exactly "*" restores the legacy allow-everything behavior
// (allowAll=true) instead of being treated as an extra entry. Unset → no
// extras, allowAll=false.
func allowedOrigins(lookup Lookup) (extras []string, allowAll bool) {
	raw, _ := lookup("STREMIO_ALLOWED_ORIGINS")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, false
	}
	if raw == "*" {
		return nil, true
	}
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			extras = append(extras, part)
		}
	}
	return extras, false
}

// metadataURL resolves the Cinemeta-compatible metadata addon base URL used by
// the /bitmagnet and /torznab add-ons to turn an IMDB id into a title. Unset →
// the official Cinemeta. An explicit empty value or off/0/false/no/disabled
// turns resolution off (the add-ons then query by the raw IMDB id).
func metadataURL(lookup Lookup) string {
	v, ok := lookup("STREMIO_METADATA_URL")
	if !ok {
		return "https://v3-cinemeta.strem.io"
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "off", "0", "false", "no", "disable", "disabled":
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(v), "/")
}

// defaultTrackersURL is the curated public tracker list fetched and ranked at
// startup. Override via STREMIO_TRACKERS_URL; an empty/off value disables the
// remote fetch entirely (the embedded/cached list plus DHT/PEX still apply).
const defaultTrackersURL = "https://raw.githubusercontent.com/XIU2/TrackersListCollection/master/best.txt"

func trackersURL(lookup Lookup) string {
	v, ok := lookup("STREMIO_TRACKERS_URL")
	if !ok {
		return defaultTrackersURL
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "off", "0", "false", "no", "disable", "disabled":
		return ""
	}
	return strings.TrimSpace(v)
}
