// SPDX-FileCopyrightText: 2026 The rivulet-engine Authors
//
// SPDX-License-Identifier: MIT

package api

import (
	"os"
	"testing"
)

// TestMain keeps the pre-hardening test fixtures (httptest servers on
// loopback, Host example.com) working; security tests opt back in with
// t.Setenv.
func TestMain(m *testing.M) {
	_ = os.Setenv("RIVULET_PROXY_ALLOW_PRIVATE", "1")
	_ = os.Setenv("RIVULET_ALLOWED_HOSTS", "example.com")
	os.Exit(m.Run())
}
