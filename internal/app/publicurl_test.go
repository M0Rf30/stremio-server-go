// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package app

import "testing"

// TestPublicBaseURL pins the validation of STREMIO_PUBLIC_URL: a valid http(s)
// URL with a host is accepted (trailing slash trimmed), anything else is
// ignored so it cannot produce a malformed redirect or baseUrl.
func TestPublicBaseURL(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"unset", "", ""},
		{"https host", "https://stremio.example.com", "https://stremio.example.com"},
		{"trailing slash trimmed", "https://stremio.example.com/", "https://stremio.example.com"},
		{"http host with port and path", "http://host:11470/base", "http://host:11470/base"},
		{"no scheme ignored", "stremio.example.com", ""},
		{"wrong scheme ignored", "ftp://stremio.example.com", ""},
		{"missing host ignored", "https:///path", ""},
		{"garbage ignored", "://", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lookup := MapLookup(map[string]string{"STREMIO_PUBLIC_URL": tc.value})
			if got := publicBaseURL(lookup, "STREMIO_PUBLIC_URL"); got != tc.want {
				t.Errorf("publicBaseURL(%q) = %q; want %q", tc.value, got, tc.want)
			}
		})
	}
}
