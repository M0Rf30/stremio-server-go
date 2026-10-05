// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyOSEnv(t *testing.T) {
	t.Setenv("STREMIO_HWACCEL", "orig")
	os.Unsetenv("LOCAL_FILES_DIR")
	restore := applyOSEnv(map[string]string{
		"STREMIO_HWACCEL": "0",
		"LOCAL_FILES_DIR": "/x",
		"HTTP_PORT":       "1",
	})
	if os.Getenv("STREMIO_HWACCEL") != "0" || os.Getenv("LOCAL_FILES_DIR") != "/x" {
		t.Fatal("env not applied")
	}
	if _, ok := os.LookupEnv("HTTP_PORT"); ok {
		t.Fatal("non-allowlisted key leaked into env")
	}
	restore()
	if os.Getenv("STREMIO_HWACCEL") != "orig" {
		t.Fatal("not restored")
	}
	if _, ok := os.LookupEnv("LOCAL_FILES_DIR"); ok {
		t.Fatal("unset key not restored")
	}
}

func TestOpenLogFileFailureReported(t *testing.T) {
	var warn bytes.Buffer
	w, closeFn := openLogFile(filepath.Join(t.TempDir(), "no", "such", "log"), &warn)
	defer closeFn()
	if w != os.Stderr {
		t.Fatal("expected stderr fallback")
	}
	if !strings.Contains(warn.String(), "cannot open log file") {
		t.Fatalf("failure not reported: %q", warn.String())
	}
}
