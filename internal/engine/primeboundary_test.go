package engine

import "testing"

func TestPiecesForBytes(t *testing.T) {
	const pieceLen int64 = 16 << 20 // 16 MiB
	cases := []struct {
		name        string
		n, pieceLen int64
		want        int
	}{
		{"zero bytes floors to 1", 0, pieceLen, 1},
		{"negative bytes floors to 1", -5, pieceLen, 1},
		{"one byte needs one piece", 1, pieceLen, 1},
		{"exact piece length needs one piece", pieceLen, pieceLen, 1},
		{"one byte over a piece needs two pieces", pieceLen + 1, pieceLen, 2},
		{"exact two piece lengths needs two pieces", 2 * pieceLen, pieceLen, 2},
		{"non-positive piece length floors to 1", 5, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := piecesForBytes(tc.n, tc.pieceLen); got != tc.want {
				t.Errorf("piecesForBytes(%d, %d) = %d; want %d", tc.n, tc.pieceLen, got, tc.want)
			}
		})
	}
}

// TestPrimeBoundaryRange is the acceptance test for the byte-windowed
// primeBoundary rewrite: on a large file with 16 MiB pieces, the default
// headWindowBytes (4 MiB) and tailWindowBytes (8 MiB) windows must each
// collapse to exactly one piece — not the old fixed 8-piece (~128 MiB) window
// that pinned enough pieces at PiecePriorityNow to compete with the reader's
// own current piece under anacrolix's equal-priority tie-break.
func TestPrimeBoundaryRange(t *testing.T) {
	const pieceLen int64 = 16 << 20 // 16 MiB
	const fileSize int64 = 60 << 30 // 60 GiB
	numPieces := int(fileSize / pieceLen)
	begin, end := 0, numPieces

	t.Run("60 GiB file, 16 MiB pieces: default windows mark exactly 1 head + 1 tail piece", func(t *testing.T) {
		headEnd, tailBegin := primeBoundaryRange(begin, end, pieceLen, headWindowBytes, tailWindowBytes)
		gotHead := headEnd - begin
		gotTail := end - tailBegin
		if gotHead != 1 {
			t.Errorf("head pieces = %d; want 1 (headEnd=%d, begin=%d)", gotHead, headEnd, begin)
		}
		if gotTail != 1 {
			t.Errorf("tail pieces = %d; want 1 (tailBegin=%d, end=%d)", gotTail, tailBegin, end)
		}
		// The two windows must not overlap on a file this large, and must sit
		// at the very front/back of the piece range.
		if headEnd != begin+1 {
			t.Errorf("headEnd = %d; want %d", headEnd, begin+1)
		}
		if tailBegin != end-1 {
			t.Errorf("tailBegin = %d; want %d", tailBegin, end-1)
		}
		if headEnd > tailBegin {
			t.Errorf("head window (< %d) overlaps tail window (>= %d) unexpectedly", headEnd, tailBegin)
		}
	})

	cases := []struct {
		name                     string
		begin, end               int
		headBytes, tailBytes     int64
		wantHeadPieces, wantTail int
	}{
		{
			name: "head window spanning two pieces", begin: 0, end: numPieces,
			headBytes: pieceLen + 1, tailBytes: tailWindowBytes,
			wantHeadPieces: 2, wantTail: 1,
		},
		{
			name: "tiny file: windows collapse and overlap the whole file", begin: 100, end: 101,
			headBytes: headWindowBytes, tailBytes: tailWindowBytes,
			wantHeadPieces: 1, wantTail: 1,
		},
		{
			name: "empty piece range is a no-op", begin: 5, end: 5,
			headBytes: headWindowBytes, tailBytes: tailWindowBytes,
			wantHeadPieces: 0, wantTail: 0,
		},
		{
			name: "windows exactly covering a 2-piece file don't exceed it", begin: 0, end: 2,
			headBytes: pieceLen, tailBytes: pieceLen,
			wantHeadPieces: 1, wantTail: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headEnd, tailBegin := primeBoundaryRange(tc.begin, tc.end, pieceLen, tc.headBytes, tc.tailBytes)
			if got := headEnd - tc.begin; got != tc.wantHeadPieces {
				t.Errorf("head pieces = %d; want %d", got, tc.wantHeadPieces)
			}
			if got := tc.end - tailBegin; got != tc.wantTail {
				t.Errorf("tail pieces = %d; want %d", got, tc.wantTail)
			}
			if headEnd < tc.begin || headEnd > tc.end {
				t.Errorf("headEnd %d out of range [%d,%d]", headEnd, tc.begin, tc.end)
			}
			if tailBegin < tc.begin || tailBegin > tc.end {
				t.Errorf("tailBegin %d out of range [%d,%d]", tailBegin, tc.begin, tc.end)
			}
		})
	}
}
