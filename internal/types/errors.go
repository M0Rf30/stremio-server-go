// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package types

import "errors"

// ErrTooManyEngines is returned by EnsureEngine when the active-engine cap is
// reached and no idle engine can be evicted to make room.
var ErrTooManyEngines = errors.New("too many active engines")
