// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package engine

import (
	"os"
	"path/filepath"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"github.com/M0Rf30/stremio-server-go/internal/logging"
)

// newFileStorage is storage.NewFileByInfoHash (data under <dir>/<infohash>,
// default per-directory piece-completion database) with the file I/O backend
// chosen explicitly instead of through the process environment.
//
// mmap=false (the default) uses classic pread/pwrite. With mmap, every byte
// streamed stays mapped into the process, so RSS grows with the amount of a
// movie watched (measured: 163 MB RSS after a 123 MB file, 123 MB of it the
// mapped file, versus a flat ~40 MB with classic). Those pages are reclaimable
// page cache, but they count against container/cgroup limits, Android's
// low-memory killer and anything reading RSS, and 32-bit builds cannot map
// files of 4 GiB or more at all. Classic costs ~100 ms of CPU per GB read
// (under 1 ms/s for a 4K stream) at equivalent throughput.
func newFileStorage(dir string, mmap bool) storage.ClientImplCloser {
	// Mirrors anacrolix's pieceCompletionForDir: create the dir before the
	// completion database opens inside it, fall back to in-memory completion.
	_ = os.MkdirAll(dir, 0o700)
	pc, err := storage.NewDefaultPieceCompletionForDir(dir)
	if err != nil {
		logging.For("engine").Warn("piece completion db unavailable; using in-memory completion", "dir", dir, "err", err)
		pc = storage.NewMapPieceCompletion()
	}
	return storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir: dir,
		TorrentDirMaker: func(baseDir string, _ *metainfo.Info, ih metainfo.Hash) string {
			return filepath.Join(baseDir, ih.HexString())
		},
		PieceCompletion: pc,
		DisableMmap:     !mmap,
	})
}
