// SPDX-FileCopyrightText: 2026 The rivulet-engine Authors
//
// SPDX-License-Identifier: MIT

package app

import (
	"os"
	"path/filepath"

	"github.com/rivulet-kodi/rivulet-engine/internal/logging"
)

const (
	defaultAppDirName = ".rivulet-engine"
	legacyAppDirName  = ".stremio-server" // default data dir before the rename
)

// defaultAppPath returns the default APP_PATH under home, moving the data
// directory of a pre-rename install (~/.stremio-server: settings, HTTPS
// certificate, caches) to ~/.rivulet-engine so an upgrade keeps it. The move
// only happens when the new directory is absent or empty (a service manager
// may have pre-created it). If the move fails the legacy directory is
// returned instead, so the data is never orphaned.
func defaultAppPath(home string) string {
	current := filepath.Join(home, defaultAppDirName)
	legacy := filepath.Join(home, legacyAppDirName)

	if fi, err := os.Stat(legacy); err != nil || !fi.IsDir() {
		return current
	}
	if entries, err := os.ReadDir(current); err == nil {
		if len(entries) > 0 {
			return current
		}
		if err := os.Remove(current); err != nil {
			return legacy
		}
	} else if !os.IsNotExist(err) {
		return current
	}
	if err := os.Rename(legacy, current); err != nil {
		logging.For("config").Warn("cannot move legacy data directory; keeping it",
			"from", legacy, "to", current, "err", err)
		return legacy
	}
	logging.For("config").Info("moved legacy data directory", "from", legacy, "to", current)
	return current
}
