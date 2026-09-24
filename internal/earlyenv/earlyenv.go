// Package earlyenv adjusts process environment that third-party packages read
// in their own init() functions, before those init() functions run.
//
// anacrolix/torrent's storage package picks its file I/O backend once, in
// init(), from TORRENT_STORAGE_DEFAULT_FILE_IO, defaulting to memory-mapping
// every torrent file whole. On a 32-bit build (android/arm, linux/arm) the
// process address space is at most 4 GiB, so any file of 4 GiB or more cannot
// be mapped: mmap-go returns a mapping whose length is the size truncated to
// 32 bits and the write fails with "new mmap has wrong size 802453308,
// expected 9392387900". anacrolix then disables data download for the whole
// torrent and every read returns "torrent data downloading disabled" -- the
// stream never starts. Files a little under 4 GiB fail too, because there is
// rarely that much contiguous free address space next to Kodi's own mappings.
// The "classic" backend uses plain pread/pwrite and has no such limit.
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

import (
	"math/bits"
	"os"
)

// FileIoEnv is the variable anacrolix/torrent/storage reads in init().
const FileIoEnv = "TORRENT_STORAGE_DEFAULT_FILE_IO"

// Applied reports whether init() forced the classic backend, for logging.
var Applied bool

func init() {
	Applied = apply(bits.UintSize, os.LookupEnv, os.Setenv)
}

// apply forces the classic file I/O backend on 32-bit builds unless the user
// already chose one explicitly. Split out from init() for testing.
func apply(wordBits int, lookup func(string) (string, bool), set func(string, string) error) bool {
	if wordBits > 32 {
		return false
	}
	if _, ok := lookup(FileIoEnv); ok {
		return false
	}
	return set(FileIoEnv, "classic") == nil
}
