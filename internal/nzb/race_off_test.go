// SPDX-FileCopyrightText: 2026 The rivulet-engine Authors
//
// SPDX-License-Identifier: MIT

//go:build !race

package nzb

// raceEnabled reports whether the test binary was built with -race (see
// race_on_test.go).
const raceEnabled = false
