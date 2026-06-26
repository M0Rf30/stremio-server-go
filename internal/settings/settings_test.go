package settings_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/M0Rf30/stremio-server-go/internal/settings"
	"github.com/M0Rf30/stremio-server-go/internal/types"
)

func cfg(appPath string) types.Config {
	return types.Config{
		Version:   "4.21.0",
		AppPath:   appPath,
		CacheRoot: filepath.Join(appPath, "cache"),
	}
}

func TestDefaults(t *testing.T) {
	dir := t.TempDir()
	ss, err := settings.New(cfg(dir))
	if err != nil {
		t.Fatal(err)
	}
	vals := ss.Values()
	if vals["serverVersion"] != "4.21.0" {
		t.Errorf("serverVersion = %v", vals["serverVersion"])
	}
	if vals["cacheSize"] != int64(10737418240) {
		t.Errorf("cacheSize = %v (%T)", vals["cacheSize"], vals["cacheSize"])
	}
	if vals["transcodeProfile"] != nil {
		t.Errorf("transcodeProfile should be nil, got %v", vals["transcodeProfile"])
	}
	if vals["transcodeHorsepower"] != float64(0.75) {
		t.Errorf("transcodeHorsepower = %v", vals["transcodeHorsepower"])
	}
	if vals["localAddonEnabled"] != false {
		t.Errorf("localAddonEnabled should be false")
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := cfg(dir)

	ss, err := settings.New(c)
	if err != nil {
		t.Fatal(err)
	}

	// Mutate and persist.
	ss.Extend(map[string]interface{}{"btMaxConnections": float64(99)})
	if err := ss.Save(); err != nil {
		t.Fatal(err)
	}

	// Reload.
	ss2, err := settings.New(c)
	if err != nil {
		t.Fatal(err)
	}
	if ss2.Get("btMaxConnections") != float64(99) {
		t.Errorf("after reload btMaxConnections = %v", ss2.Get("btMaxConnections"))
	}
}

func TestCfgPinnedFieldsOnLoad(t *testing.T) {
	dir := t.TempDir()
	c := cfg(dir)

	// Write a settings file that tries to override pinned fields.
	file := map[string]interface{}{
		"appPath":          "/evil",
		"cacheRoot":        "/evil",
		"serverVersion":    "0.0.0",
		"btMaxConnections": 77,
	}
	data, _ := json.Marshal(file)
	os.WriteFile(filepath.Join(dir, "server-settings.json"), data, 0o644)

	ss, err := settings.New(c)
	if err != nil {
		t.Fatal(err)
	}
	if ss.Get("appPath") != dir {
		t.Errorf("appPath should be pinned to %s, got %v", dir, ss.Get("appPath"))
	}
	if ss.Get("serverVersion") != "4.21.0" {
		t.Errorf("serverVersion should be pinned, got %v", ss.Get("serverVersion"))
	}
	// Non-pinned fields from file should win.
	if ss.Get("btMaxConnections") != float64(77) {
		t.Errorf("btMaxConnections from file = %v", ss.Get("btMaxConnections"))
	}
}

func TestOptionsSchema(t *testing.T) {
	dir := t.TempDir()
	ss, _ := settings.New(cfg(dir))

	schema := ss.OptionsSchema([]string{"192.168.1.1", "2001:db8::1"})
	if len(schema) != 3 {
		t.Fatalf("schema len = %d, want 3", len(schema))
	}

	// First: localAddonEnabled checkbox
	if schema[0]["id"] != "localAddonEnabled" || schema[0]["type"] != "checkbox" {
		t.Errorf("first element wrong: %v", schema[0])
	}

	// Second: remoteHttps select
	httpsSels := schema[1]["selections"].([]map[string]interface{})
	if len(httpsSels) != 3 {
		t.Errorf("remoteHttps selections len = %d, want 3", len(httpsSels))
	}
	if httpsSels[0]["name"] != "Disabled" || httpsSels[0]["val"] != "" {
		t.Errorf("first remoteHttps selection wrong: %v", httpsSels[0])
	}

	// Third: cacheSize select — last entry val must be nil (∞)
	cacheSels := schema[2]["selections"].([]map[string]interface{})
	if len(cacheSels) != 5 {
		t.Errorf("cacheSize selections len = %d, want 5", len(cacheSels))
	}
	last := cacheSels[len(cacheSels)-1]
	if last["name"] != "∞" || last["val"] != nil {
		t.Errorf("last cacheSize selection wrong: %v", last)
	}
}

func TestAtomicSave(t *testing.T) {
	dir := t.TempDir()
	ss, _ := settings.New(cfg(dir))
	ss.Extend(map[string]interface{}{"proxyStreamsEnabled": true})
	if err := ss.Save(); err != nil {
		t.Fatal(err)
	}
	// Temp file should not be left behind.
	if _, err := os.Stat(filepath.Join(dir, "server-settings.json.tmp")); !os.IsNotExist(err) {
		t.Error(".tmp file left behind after save")
	}
	// The real file should exist and be valid JSON.
	data, err := os.ReadFile(filepath.Join(dir, "server-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("saved file is not valid JSON: %v", err)
	}
}
