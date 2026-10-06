// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"fmt"
	"io"
	"testing"
)

type tailStep struct {
	n   int
	err error
}

// scriptedTailReader replays a fixed list of ReadContext results and then
// keeps returning io.EOF, like an exhausted reader.
type scriptedTailReader struct {
	steps []tailStep
	calls int
}

func (s *scriptedTailReader) ReadContext(_ context.Context, _ []byte) (int, error) {
	i := s.calls
	s.calls++
	if i >= len(s.steps) {
		return 0, io.EOF
	}
	return s.steps[i].n, s.steps[i].err
}

// TestDrainTail pins what warmMoov treats as a finished tail pre-read. Reaching
// EOF means the tail was delivered: only a read that fails or stalls first is an
// abandoned warm (which demotes the file and clears the once-only marker so the
// next reader retries). Before this distinction a clean EOF was handled like a
// timeout, so every NewReader re-ran the tail warm.
func TestDrainTail(t *testing.T) {
	cases := []struct {
		name  string
		steps []tailStep
		want  bool
	}{
		{"data then clean EOF", []tailStep{{65536, nil}, {65536, nil}, {0, io.EOF}}, true},
		{"EOF alone", []tailStep{{0, io.EOF}}, true},
		{"final bytes delivered together with EOF", []tailStep{{100, io.EOF}}, true},
		{"wrapped EOF", []tailStep{{10, nil}, {0, fmt.Errorf("read tail: %w", io.EOF)}}, true},
		{"deadline exceeded mid-tail", []tailStep{{65536, nil}, {0, context.DeadlineExceeded}}, false},
		{"cancelled (torrent dropped)", []tailStep{{0, context.Canceled}}, false},
		{"unexpected EOF", []tailStep{{10, nil}, {0, io.ErrUnexpectedEOF}}, false},
		{"no progress and no error", []tailStep{{10, nil}, {0, nil}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &scriptedTailReader{steps: tc.steps}
			if got := drainTail(context.Background(), r); got != tc.want {
				t.Errorf("drainTail = %v; want %v", got, tc.want)
			}
		})
	}
}
