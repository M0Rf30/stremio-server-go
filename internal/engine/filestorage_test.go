// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
)

// mappedAfterWrite writes and reads one piece through newFileStorage and
// reports whether the torrent file appears in this process's mappings.
func mappedAfterWrite(t *testing.T, mmap bool, name string) bool {
	t.Helper()
	const pieceLen = 256 << 10
	info := &metainfo.Info{Name: name, Length: 4 * pieceLen, PieceLength: pieceLen, Pieces: make([]byte, 20*4)}
	dir := t.TempDir()
	st := newFileStorage(dir, mmap)
	t.Cleanup(func() { _ = st.Close() })
	ih := metainfo.Hash{3}
	tor, err := st.OpenTorrent(context.Background(), info, ih)
	if err != nil {
		t.Fatalf("OpenTorrent: %v", err)
	}
	t.Cleanup(func() {
		if tor.Close != nil {
			_ = tor.Close()
		}
	})
	p := tor.Piece(info.Piece(1))
	if _, err := p.WriteAt(make([]byte, pieceLen), 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := p.ReadAt(make([]byte, 16), 0); err != nil {
		t.Fatalf("read: %v", err)
	}
	// Data lands under <dir>/<infohash>, like storage.NewFileByInfoHash.
	if _, err := os.Stat(filepath.Join(dir, ih.HexString())); err != nil {
		t.Fatalf("expected infohash-partitioned layout: %v", err)
	}
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(maps), name)
}

// TestFileStorageClassicByDefault checks the effect, not a flag: with the
// default (mmap=false) the torrent file must not be mapped into the process;
// with mmap=true it is (on 64-bit, where the mmap backend is available).
func TestFileStorageClassicByDefault(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc/self/maps")
	}
	if mappedAfterWrite(t, false, "probe-classic.mkv") {
		t.Fatal("default file storage memory-maps torrent files")
	}
	if runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64" {
		if !mappedAfterWrite(t, true, "probe-mmap.mkv") {
			t.Fatal("mmap=true did not map the file: the detection would miss a regression")
		}
	}
}
