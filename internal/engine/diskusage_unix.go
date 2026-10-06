// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package engine

import (
	"io/fs"
	"syscall"
)

// allocatedSize reports the bytes a file really occupies on disk. The torrent
// file storage creates every file at its full length up front and fills it in
// piece by piece, so a half-downloaded 2 GiB movie has an apparent size
// (info.Size()) of 2 GiB while using a few hundred MiB of blocks. Charging the
// apparent size made evict() think the cache was full as soon as one large
// torrent was opened and drop every other (barely used) one. The result is
// capped at the apparent size so block-rounding never reports more than the
// file's length.
func allocatedSize(info fs.FileInfo) int64 {
	size := info.Size()
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		// Blocks is always counted in 512-byte units, regardless of the
		// filesystem block size.
		if alloc := st.Blocks * 512; alloc >= 0 && alloc < size {
			return alloc
		}
	}
	return size
}
