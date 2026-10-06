// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package engine

import "io/fs"

// allocatedSize falls back to the apparent size where the allocated block
// count is not available through os.FileInfo (e.g. Windows); see the unix
// variant for why the distinction matters to evict().
func allocatedSize(info fs.FileInfo) int64 {
	return info.Size()
}
