// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

// Package earlyenv adjusts process environment that third-party packages read
// in their own init() functions, before those init() functions run.
//
// anacrolix/torrent's storage package picks its file I/O backend once, in
// init(), from TORRENT_STORAGE_DEFAULT_FILE_IO, defaulting to memory-mapping
// every torrent file whole. This package makes the "classic" backend (plain
// pread/pwrite) the default on every platform, for two reasons:
//
//   - Correctness on 32-bit (android/arm, linux/arm): the address space is at
//     most 4 GiB, so files of 4 GiB or more cannot be mapped -- mmap-go
//     returns a mapping truncated to 32 bits, the write fails with "new mmap
//     has wrong size", anacrolix disables data download for the torrent and
//     the stream never starts. Files a little under 4 GiB fail too, as there
//     is rarely that much contiguous free address space.
//   - Memory on 64-bit: with mmap every byte streamed stays mapped into the
//     process, so RSS grows with the amount of the movie watched (measured:
//     163 MB RSS after a 123 MB file, of which 123 MB was the mapped file,
//     versus a flat ~40 MB with classic). The pages are reclaimable page
//     cache, but they count against container/cgroup limits, Android's
//     low-memory killer and anything reading RSS -- a 20 GB remux shows up
//     as GBs of "RAM". classic costs ~100 ms of CPU per GB read (under 1 ms
//     per second for a 4K stream) at equivalent throughput.
//
// An explicit TORRENT_STORAGE_DEFAULT_FILE_IO (e.g. "mmap") is respected.
//
// Ordering: since Go 1.21 the spec initializes packages by repeatedly picking
// the first package, sorted by import path, whose dependencies are already
// initialized. This package depends only on the standard library, and
// "github.com/M0Rf30/..." sorts before "github.com/anacrolix/...", so its
// init() runs before anacrolix/torrent/storage's. TestInitOrder guards that.
//
// Importing it from internal/app covers both the executable and the c-shared
// library (whose runtime, and therefore every init(), starts at dlopen).
package earlyenv

import "os"

// FileIoEnv is the variable anacrolix/torrent/storage reads in init().
const FileIoEnv = "TORRENT_STORAGE_DEFAULT_FILE_IO"

// Applied reports whether init() selected the classic backend (false when
// the user set FileIoEnv explicitly), for logging.
var Applied bool

func init() {
	Applied = apply(os.LookupEnv, os.Setenv)
}

// apply selects the classic file I/O backend unless the user already chose
// one explicitly. Split out from init() for testing.
func apply(lookup func(string) (string, bool), set func(string, string) error) bool {
	if _, ok := lookup(FileIoEnv); ok {
		return false
	}
	return set(FileIoEnv, "classic") == nil
}
