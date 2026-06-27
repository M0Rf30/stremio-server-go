// Package settings implements types.SettingsStore: it holds the user-facing
// server settings, persists them to <appPath>/server-settings.json, and
// exposes the GUI schema expected by stremio-web's settings panel.
package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/M0Rf30/stremio-server-go/internal/types"
)

const settingsFile = "server-settings.json"

// store is the concrete implementation of types.SettingsStore.
type store struct {
	mu      sync.Mutex
	values  map[string]interface{}
	appPath string // used for Save(); kept separately to avoid a map look-up under lock
}

// New creates a SettingsStore seeded with defaults, then merges any previously
// saved settings from <cfg.AppPath>/server-settings.json.  cfg-pinned fields
// (appPath, cacheRoot, serverVersion) always reflect the current cfg — they are
// never overwritten by the persisted file.
func New(cfg types.Config) (types.SettingsStore, error) {
	s := &store{
		appPath: cfg.AppPath,
		values:  makeDefaults(cfg),
	}

	path := filepath.Join(cfg.AppPath, settingsFile)
	data, err := os.ReadFile(path)
	if err == nil {
		// File exists — unmarshal and overlay onto defaults.
		var loaded map[string]interface{}
		if jsonErr := json.Unmarshal(data, &loaded); jsonErr == nil {
			for k, v := range loaded {
				s.values[k] = v
			}
		}
		// Malformed JSON is silently ignored; defaults remain intact.

		// cfg-pinned fields always win regardless of what was persisted.
		s.values["appPath"] = cfg.AppPath
		s.values["cacheRoot"] = cfg.CacheRoot
		s.values["serverVersion"] = cfg.Version
	}
	// A missing file (os.IsNotExist) is not an error; any other OS error is
	// surfaced so the caller can decide.
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("settings: read %s: %w", path, err)
	}

	return s, nil
}

// makeDefaults returns a fresh map with the canonical settings.values defaults
// reported by GET /settings.
func makeDefaults(cfg types.Config) map[string]interface{} {
	return map[string]interface{}{
		"serverVersion":             cfg.Version,
		"appPath":                   cfg.AppPath,
		"cacheRoot":                 cfg.CacheRoot,
		"cacheSize":                 int64(10737418240), // 10 GB (video-sized default; 2 GB churned on large torrents)
		"btMaxConnections":          int(55),
		"btHandshakeTimeout":        int(20000),
		"btRequestTimeout":          int(4000),
		"btDownloadSpeedSoftLimit":  int(2621440),
		"btDownloadSpeedHardLimit":  int(3670016),
		"btMinPeersForStable":       int(5),
		"remoteHttps":               "",
		"localAddonEnabled":         false,
		"transcodeHorsepower":       float64(0.75),
		"transcodeMaxBitRate":       int(0),
		"transcodeConcurrency":      int(1),
		"transcodeTrackConcurrency": int(1),
		"transcodeHardwareAccel":    true,
		"transcodeProfile":          nil,        // null in JSON
		"allTranscodeProfiles":      []string{}, // [] in JSON
		"transcodeMaxWidth":         int(1920),
		"proxyStreamsEnabled":       false,
		// --- seeding behaviour (mirrors Rust reference seedingEnabled) ---
		// When true (default) torrents continue seeding after download, improving
		// swarm health.  When false, torrents pause once their download finishes.
		"seedingEnabled": true,

		// --- tracker management (mirrors Rust reference cachedTrackers / trackersSourceUrl) ---
		// Public tracker list is fetched from trackersSourceUrl and cached in
		// cachedTrackers; trackersLastUpdated holds the Unix timestamp of the last
		// successful fetch.  These fields are engine-managed at runtime but must
		// survive Save/Load so the ranked list is not discarded on restart.
		"trackersSourceUrl":   cfg.TrackersURL,
		"cachedTrackers":      []string{},
		"trackersLastUpdated": int64(0),

		// --- update policy (mirrors Rust reference autoUpdateEnabled / updateChannel) ---
		// The Go server does not self-update, but stremio-web reads these fields
		// and may surface them in the UI.  Defaults match the Rust reference.
		"autoUpdateEnabled":        true,
		"updateChannel":            "",
		"updateCheckIntervalHours": int(6),
	}
}

// Values returns a shallow copy of the current settings map (safe to mutate).
func (s *store) Values() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make(map[string]interface{}, len(s.values))
	for k, v := range s.values {
		cp[k] = v
	}
	return cp
}

// Get returns the value for a single key (nil if absent).
func (s *store) Get(key string) interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.values[key]
}

// Extend merges patch keys into the values map (POST /settings body).
func (s *store) Extend(patch map[string]interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range patch {
		s.values[k] = v
	}
}

// Save atomically writes the current values as pretty-printed JSON to
// <appPath>/server-settings.json (write to .tmp then os.Rename).
func (s *store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.MarshalIndent(s.values, "", "  ")
	if err != nil {
		return fmt.Errorf("settings: marshal: %w", err)
	}

	appPath := s.appPath
	if err := os.MkdirAll(appPath, 0o755); err != nil {
		return fmt.Errorf("settings: mkdir %s: %w", appPath, err)
	}

	dst := filepath.Join(appPath, settingsFile)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("settings: write temp: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp) // best-effort cleanup
		return fmt.Errorf("settings: rename to %s: %w", dst, err)
	}
	return nil
}

// OptionsSchema returns the three-element GUI schema array that stremio-web
// renders in its settings panel.  availableInterfaces is the list of IP
// addresses returned by GET /network-info.
func (s *store) OptionsSchema(availableInterfaces []string) []map[string]interface{} {
	// remoteHttps selector: "Disabled" first, then one entry per interface.
	httpsSels := make([]map[string]interface{}, 0, 1+len(availableInterfaces))
	httpsSels = append(httpsSels, map[string]interface{}{"name": "Disabled", "val": ""})
	for _, ip := range availableInterfaces {
		httpsSels = append(httpsSels, map[string]interface{}{"name": ip, "val": ip})
	}

	// cacheSize selector: null (nil) encodes as JSON null (= unlimited).
	cacheSels := []map[string]interface{}{
		{"name": "no caching", "val": int64(0)},
		{"name": "2GB", "val": int64(2147483648)},
		{"name": "5GB", "val": int64(5368709120)},
		{"name": "10GB", "val": int64(10737418240)},
		{"name": "∞", "val": nil},
	}

	return []map[string]interface{}{
		{
			"id":    "localAddonEnabled",
			"label": "ENABLE_LOCAL_FILES_ADDON",
			"type":  "checkbox",
		},
		{
			"id":         "remoteHttps",
			"label":      "ENABLE_REMOTE_HTTPS_CONN",
			"type":       "select",
			"class":      "https",
			"icon":       true,
			"selections": httpsSels,
		},
		{
			"id":         "cacheSize",
			"label":      "CACHING",
			"type":       "select",
			"class":      "caching",
			"icon":       true,
			"selections": cacheSels,
		},
	}
}
