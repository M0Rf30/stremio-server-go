// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// benchTorrent is one opened in-RAM torrent plus handles to all its pieces.
type benchTorrent struct {
	info   *metainfo.Info
	pieces []storage.PieceImpl
}

// openBenchTorrent opens a torrent with numPieces pieces on s. When fill is
// true every piece is written with a deterministic pattern and marked complete.
func openBenchTorrent(tb testing.TB, s *memStorage, id byte, pieceLen int64, numPieces int, fill bool) *benchTorrent {
	tb.Helper()
	info := syntheticInfo(pieceLen, numPieces)
	ti, err := s.OpenTorrent(context.Background(), info, metainfo.Hash{id})
	if err != nil {
		tb.Fatalf("OpenTorrent: %v", err)
	}
	bt := &benchTorrent{info: info, pieces: make([]storage.PieceImpl, numPieces)}
	for i := range bt.pieces {
		bt.pieces[i] = ti.Piece(info.Piece(i))
		if !fill {
			continue
		}
		if _, err := bt.pieces[i].WriteAt(fillPattern(i, pieceLen), 0); err != nil {
			tb.Fatalf("WriteAt: %v", err)
		}
		if err := bt.pieces[i].MarkComplete(); err != nil {
			tb.Fatalf("MarkComplete: %v", err)
		}
	}
	return bt
}

// BenchmarkMemStorageReadAtParallel measures ReadAt throughput when many
// goroutines stream from different torrents on one shared store (the
// multi-stream case). The budget is large enough that nothing is evicted, so it
// isolates lock contention on the read path.
func BenchmarkMemStorageReadAtParallel(b *testing.B) {
	const (
		numTorrents = 16
		numPieces   = 8
		pieceLen    = 1 << 20
		readLen     = 64 << 10
	)
	s := newMemStorage(int64(numTorrents) * numPieces * pieceLen * 2)
	b.Cleanup(func() { _ = s.Close() })
	torrents := make([]*benchTorrent, numTorrents)
	for i := range torrents {
		torrents[i] = openBenchTorrent(b, s, byte(i), pieceLen, numPieces, true)
	}

	var next atomic.Int64
	b.ReportAllocs()
	b.SetBytes(readLen)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		bt := torrents[int(next.Add(1))%numTorrents]
		buf := make([]byte, readLen)
		i := 0
		for pb.Next() {
			p := bt.pieces[i%numPieces]
			off := int64(i%(pieceLen/readLen)) * readLen
			if n, err := p.ReadAt(buf, off); n != readLen || err != nil {
				b.Errorf("ReadAt = (%d, %v)", n, err)
				return
			}
			i++
		}
	})
}

// BenchmarkMemStorageReadWhileWriting is the same read workload with two
// background goroutines continuously downloading (16 KiB chunk WriteAt +
// MarkComplete) into another torrent, so reads compete with writes the way
// they do while a stream plays during a download.
func BenchmarkMemStorageReadWhileWriting(b *testing.B) {
	const (
		numTorrents = 16
		numPieces   = 8
		pieceLen    = 1 << 20
		readLen     = 64 << 10
		chunkLen    = 16 << 10
		writers     = 2
	)
	s := newMemStorage(int64(numTorrents+writers) * numPieces * pieceLen * 2)
	b.Cleanup(func() { _ = s.Close() })
	torrents := make([]*benchTorrent, numTorrents)
	for i := range torrents {
		torrents[i] = openBenchTorrent(b, s, byte(i), pieceLen, numPieces, true)
	}
	wt := make([]*benchTorrent, writers)
	for i := range wt {
		wt[i] = openBenchTorrent(b, s, byte(100+i), pieceLen, numPieces, false)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(bt *benchTorrent) {
			defer wg.Done()
			chunk := make([]byte, chunkLen)
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				p := bt.pieces[i%numPieces]
				for off := int64(0); off < pieceLen; off += chunkLen {
					if _, err := p.WriteAt(chunk, off); err != nil {
						b.Errorf("WriteAt: %v", err)
						return
					}
				}
				if err := p.MarkComplete(); err != nil {
					b.Errorf("MarkComplete: %v", err)
					return
				}
			}
		}(wt[w])
	}

	var next atomic.Int64
	b.ReportAllocs()
	b.SetBytes(readLen)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		bt := torrents[int(next.Add(1))%numTorrents]
		buf := make([]byte, readLen)
		i := 0
		for pb.Next() {
			p := bt.pieces[i%numPieces]
			off := int64(i%(pieceLen/readLen)) * readLen
			if n, err := p.ReadAt(buf, off); n != readLen || err != nil {
				b.Errorf("ReadAt = (%d, %v)", n, err)
				return
			}
			i++
		}
	})
	b.StopTimer()
	close(stop)
	wg.Wait()
}

// BenchmarkMemStorageEvictOne measures the steady-state cost of completing one
// piece when the store is full of n complete pieces, i.e. one eviction per
// iteration. Victim selection used to scan every complete piece, so the cost
// grew linearly with n.
func BenchmarkMemStorageEvictOne(b *testing.B) {
	for _, n := range []int{1 << 10, 1 << 14, 1 << 17} {
		b.Run(fmt.Sprintf("resident=%d", n), func(b *testing.B) {
			const pieceLen = 128
			s := newMemStorage(int64(n) * pieceLen)
			b.Cleanup(func() { _ = s.Close() })
			// 2n pieces in a ring: a piece is always evicted (n steps later)
			// before it is written again (2n steps later).
			bt := openBenchTorrent(b, s, 1, pieceLen, 2*n, false)
			chunk := fillPattern(1, pieceLen)
			cycle := func(i int) {
				p := bt.pieces[i%(2*n)]
				if _, err := p.WriteAt(chunk, 0); err != nil {
					b.Fatalf("WriteAt: %v", err)
				}
				if err := p.MarkComplete(); err != nil {
					b.Fatalf("MarkComplete: %v", err)
				}
			}
			for i := range n { // fill to the budget
				cycle(i)
			}
			b.ReportAllocs()
			i := n
			for b.Loop() {
				cycle(i)
				i++
			}
		})
	}
}

// BenchmarkMemStorageEvictBurst measures dropping every one of n complete
// pieces in a single eviction pass (budget shrunk to zero): O(n^2) with a
// per-victim scan, O(n log n) with an ordered structure.
func BenchmarkMemStorageEvictBurst(b *testing.B) {
	for _, n := range []int{1 << 11, 1 << 13} {
		b.Run(fmt.Sprintf("pieces=%d", n), func(b *testing.B) {
			const pieceLen = 128
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				s := newMemStorage(int64(n) * pieceLen)
				openBenchTorrent(b, s, 1, pieceLen, n, true)
				b.StartTimer()

				s.mu.Lock()
				s.capacity = 0
				s.evictLocked(0, nil)
				left := s.used
				s.mu.Unlock()

				b.StopTimer()
				if left != 0 {
					b.Fatalf("used = %d after full eviction, want 0", left)
				}
				_ = s.Close()
				b.StartTimer()
			}
		})
	}
}
