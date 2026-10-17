// SPDX-FileCopyrightText: 2026 The rivulet-engine Authors
//
// SPDX-License-Identifier: MIT

package app

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultAppPath(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(home string)
		wantFile string // relative to the returned dir; "" = none
		wantOld  bool   // legacy dir still present afterwards
	}{
		{
			name:  "fresh install uses the new dir",
			setup: func(string) {},
		},
		{
			name: "legacy dir is moved with its data",
			setup: func(h string) {
				writeFile(t, filepath.Join(h, ".stremio-server", "server-settings.json"), "legacy")
			},
			wantFile: "server-settings.json",
		},
		{
			name: "pre-created empty new dir does not block the move",
			setup: func(h string) {
				writeFile(t, filepath.Join(h, ".stremio-server", "cert.pem"), "legacy")
				if err := os.Mkdir(filepath.Join(h, ".rivulet-engine"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			wantFile: "cert.pem",
		},
		{
			name: "non-empty new dir wins and legacy is left alone",
			setup: func(h string) {
				writeFile(t, filepath.Join(h, ".stremio-server", "old.json"), "legacy")
				writeFile(t, filepath.Join(h, ".rivulet-engine", "new.json"), "new")
			},
			wantFile: "new.json",
			wantOld:  true,
		},
		{
			name: "legacy plain file is ignored",
			setup: func(h string) {
				writeFile(t, filepath.Join(h, ".stremio-server"), "not a dir")
			},
			wantOld: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			tt.setup(home)
			got := defaultAppPath(home)
			if want := filepath.Join(home, ".rivulet-engine"); got != want {
				t.Fatalf("defaultAppPath = %q, want %q", got, want)
			}
			if tt.wantFile != "" {
				if _, err := os.Stat(filepath.Join(got, tt.wantFile)); err != nil {
					t.Errorf("expected %s in %s: %v", tt.wantFile, got, err)
				}
			}
			_, err := os.Stat(filepath.Join(home, ".stremio-server"))
			if (err == nil) != tt.wantOld {
				t.Errorf("legacy dir present = %v, want %v", err == nil, tt.wantOld)
			}
		})
	}
}
