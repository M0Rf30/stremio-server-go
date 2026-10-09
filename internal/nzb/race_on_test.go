// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

//go:build race

package nzb

// raceEnabled reports whether the test binary was built with -race, under
// which sync.Pool randomly discards a quarter of the items put back into it
// (so pooled-allocation figures are noisy).
const raceEnabled = true
