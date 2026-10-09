// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package earlyenv_test

// External test package on purpose: test files inside package earlyenv would
// make earlyenv itself depend on anacrolix/torrent/storage, forcing storage's
// init() to run first -- the opposite of the production import graph.

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"github.com/M0Rf30/stremio-server-go/internal/earlyenv"
)

// TestInitOrder proves this package's init() ran before anacrolix/torrent/
// storage's, by checking the environment storage observed: it must already
// carry a backend choice -- the forced classic one, or the runner's own.
func TestInitOrder(t *testing.T) {
	v, ok := os.LookupEnv(earlyenv.FileIoEnv)
	if !ok {
		t.Fatal("file I/O backend was not selected before anacrolix/torrent/storage init")
	}
	if earlyenv.Applied && v != "classic" {
		t.Fatalf("Applied but %s=%q", earlyenv.FileIoEnv, v)
	}
}

// TestLargeFileWritable writes into a 5 GiB torrent file through the real
// default file storage. With anacrolix's default mmap backend this fails on
// a 32-bit build with "new mmap has wrong size"; with classic I/O it is a
// sparse pwrite and succeeds everywhere.
func TestLargeFileWritable(t *testing.T) {
	const pieceLen = 256 << 10
	const length = int64(5) << 30
	numPieces := int((length + pieceLen - 1) / pieceLen)
	info := &metainfo.Info{
		Name:        "big.mkv",
		Length:      length,
		PieceLength: pieceLen,
		Pieces:      make([]byte, 20*numPieces),
	}
	client := storage.NewFileByInfoHash(t.TempDir())
	t.Cleanup(func() { _ = client.Close() })
	tor, err := client.OpenTorrent(context.Background(), info, metainfo.Hash{1})
	if err != nil {
		t.Fatalf("OpenTorrent: %v", err)
	}
	t.Cleanup(func() {
		if tor.Close != nil {
			_ = tor.Close()
		}
	})
	last := info.Piece(numPieces - 1)
	if _, err := tor.Piece(last).WriteAt([]byte("tail"), 0); err != nil {
		t.Fatalf("write past 4 GiB: %v", err)
	}
	if _, err := tor.Piece(info.Piece(0)).WriteAt([]byte("head"), 0); err != nil {
		t.Fatalf("write at start: %v", err)
	}
}

// TestDefaultBackendDoesNotMapFiles checks the effect, not just the env var:
// after writing through anacrolix's default file storage, the torrent file
// must not appear in this process's memory mappings (it would with mmap).
func TestDefaultBackendDoesNotMapFiles(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc/self/maps")
	}
	if v, _ := os.LookupEnv(earlyenv.FileIoEnv); v != "classic" {
		t.Skipf("runner selected %s=%q explicitly", earlyenv.FileIoEnv, v)
	}
	const pieceLen = 256 << 10
	info := &metainfo.Info{Name: "probe-unmapped.mkv", Length: 4 * pieceLen, PieceLength: pieceLen, Pieces: make([]byte, 20*4)}
	client := storage.NewFileByInfoHash(t.TempDir())
	t.Cleanup(func() { _ = client.Close() })
	tor, err := client.OpenTorrent(context.Background(), info, metainfo.Hash{2})
	if err != nil {
		t.Fatalf("OpenTorrent: %v", err)
	}
	t.Cleanup(func() {
		if tor.Close != nil {
			_ = tor.Close()
		}
	})
	if _, err := tor.Piece(info.Piece(1)).WriteAt(make([]byte, pieceLen), 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 16)
	if _, err := tor.Piece(info.Piece(1)).ReadAt(buf, 0); err != nil {
		t.Fatalf("read: %v", err)
	}
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(maps), "probe-unmapped.mkv") {
		t.Fatal("torrent file is memory-mapped: classic file I/O is not in effect")
	}
}
