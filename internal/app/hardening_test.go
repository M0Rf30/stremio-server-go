// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelfHostFor(t *testing.T) {
	cases := map[string]string{
		"":          "127.0.0.1",
		"0.0.0.0":   "127.0.0.1",
		"::":        "127.0.0.1",
		"127.0.0.1": "127.0.0.1",
		"127.0.0.2": "127.0.0.2",
		"::1":       "::1",
		"192.0.2.1": "127.0.0.1",
	}
	for in, want := range cases {
		if got := selfHostFor(in); got != want {
			t.Errorf("selfHostFor(%q)=%q want %q", in, got, want)
		}
	}
}

func TestProxySecretEmptyFileRegenerates(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "proxy-secret")
	if err := os.WriteFile(f, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lookup := func(string) (string, bool) { return "", false }
	s, err := proxySecret(dir, lookup)
	if err != nil || s == "" {
		t.Fatalf("secret=%q err=%v", s, err)
	}
	data, _ := os.ReadFile(f)
	if strings.TrimSpace(string(data)) != s {
		t.Fatalf("file not rewritten: %q", data)
	}
}

func TestRedactProxyURL(t *testing.T) {
	got := redactProxyURL("socks5://user:hunter2@proxy:1080")
	if strings.Contains(got, "hunter2") {
		t.Fatalf("password leaked: %s", got)
	}
	if redactProxyURL("socks5://proxy:1080") != "socks5://proxy:1080" {
		t.Fatal("plain URL changed")
	}
}
