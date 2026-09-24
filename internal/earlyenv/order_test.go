package earlyenv_test

// External test package on purpose: test files inside package earlyenv would
// make earlyenv itself depend on anacrolix/torrent/storage, forcing storage's
// init() to run first -- the opposite of the production import graph.

import (
	"context"
	"math/bits"
	"os"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"github.com/M0Rf30/stremio-server-go/internal/earlyenv"
)

// TestInitOrder proves this package's init() ran before anacrolix/torrent/
// storage's, by checking the environment storage observed: on 32-bit it must
// already carry the forced value (unless the runner set one explicitly).
func TestInitOrder(t *testing.T) {
	if bits.UintSize > 32 {
		t.Skip("only meaningful on 32-bit builds; run with GOARCH=386")
	}
	if !earlyenv.Applied {
		if _, ok := os.LookupEnv(earlyenv.FileIoEnv); !ok {
			t.Fatal("32-bit build but classic file I/O was not forced")
		}
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
