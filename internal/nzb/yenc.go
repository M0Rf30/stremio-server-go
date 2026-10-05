// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package nzb

import (
	"bufio"
	"bytes"
	"fmt"
	"hash/crc32"
	"io"
	"strconv"
	"strings"
)

// DecodeYenc decodes a single-part yEnc-encoded article body from r and writes
// the decoded bytes to w.
//
// yEnc encoding: each original byte b is stored as (b + 42) % 256. Four values
// in the encoded stream (NUL, CR, LF, '=') must be further escaped: the escape
// character '=' is emitted, followed by (b + 42 + 64) % 256. Decoding reverses
// this: a plain byte p decodes to (p - 42) % 256; an escaped byte e (after '=')
// decodes to (e - 64 - 42) % 256.
//
// Line endings added by the encoder for line-length management are not part of
// the original data and are discarded by the scanner.
//
// The article must be well-formed: DecodeYenc returns an error when no
// =ybegin header or no =yend trailer is found (error text, other encodings,
// truncated body), when the decoded length differs from the trailer's size=,
// or when a pcrc32= (or, for single-part articles, crc32=) is present and does
// not match. Callers rely on this so a bad article is never treated as data.
func DecodeYenc(r io.Reader, w io.Writer) error {
	// Batch per-line Write calls through a 128 KiB buffer to reduce the number
	// of write syscalls (was one syscall per ~128-byte encoded line before).
	bw := bufio.NewWriterSize(w, 128*1024)

	const maxLine = 256 * 1024
	scanner := bufio.NewScanner(r)
	// Large buffer avoids re-allocation for long lines and amortises reads.
	scanner.Buffer(make([]byte, maxLine), maxLine)

	// outBuf is reused across all lines in this call: after the first line the
	// backing array is kept and only reset to length 0, so allocation 3 from
	// the old make([]byte, 0, len(raw)) per-line drops to ~zero.
	outBuf := make([]byte, 0, 256)
	inBody := false
	sawPart := false
	var written int64
	crc := crc32.NewIEEE()

	for scanner.Scan() {
		// scanner.Bytes() is a zero-copy view into the scanner's internal
		// buffer, valid until the next Scan(). Replaces scanner.Text() +
		// []byte(line) which allocated two heap objects per line.
		raw := scanner.Bytes()

		switch {
		case bytes.HasPrefix(raw, []byte("=ybegin")):
			inBody = true
			continue
		case bytes.HasPrefix(raw, []byte("=ypart")):
			// multi-part header: still inside the data section
			sawPart = true
			continue
		case bytes.HasPrefix(raw, []byte("=yend")):
			// end-of-part marker; flush the buffer, verify and stop decoding.
			if !inBody {
				return fmt.Errorf("yenc: =yend without =ybegin")
			}
			if err := bw.Flush(); err != nil {
				return err
			}
			return verifyYencTrailer(string(raw), written, crc.Sum32(), sawPart)
		}

		if !inBody {
			continue
		}

		outBuf = outBuf[:0]
		for i := 0; i < len(raw); i++ {
			b := raw[i]
			if b == '=' {
				i++
				if i >= len(raw) {
					return fmt.Errorf("yenc: escape character at end of line")
				}
				// escaped byte: subtract 64 + 42 (wraps as uint8)
				outBuf = append(outBuf, raw[i]-64-42)
			} else {
				// plain byte: subtract 42 (wraps as uint8)
				outBuf = append(outBuf, b-42)
			}
		}

		if _, err := bw.Write(outBuf); err != nil {
			return err
		}
		written += int64(len(outBuf))
		_, _ = crc.Write(outBuf)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	// Whatever was decoded is still flushed, but the article is not usable.
	_ = bw.Flush()
	if !inBody {
		return fmt.Errorf("yenc: no =ybegin header found (not a yEnc article)")
	}
	return fmt.Errorf("yenc: no =yend trailer found (truncated article)")
}

// verifyYencTrailer checks the decoded part length and CRC against the values
// declared on the =yend line. Absent or unparsable fields are not enforced,
// except that a size= that parses must match exactly.
func verifyYencTrailer(line string, written int64, sum uint32, multipart bool) error {
	var crcStr string
	for _, f := range strings.Fields(line)[1:] {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		switch strings.ToLower(k) {
		case "size":
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n != written {
				return fmt.Errorf("yenc: decoded %d bytes, =yend declares size=%d", written, n)
			}
		case "pcrc32":
			crcStr = v
		case "crc32":
			// For a multi-part article crc32= covers the whole file, not this
			// part; only trust it for single-part articles.
			if !multipart && crcStr == "" {
				crcStr = v
			}
		}
	}
	if crcStr == "" {
		return nil
	}
	want, err := strconv.ParseUint(crcStr, 16, 32)
	if err != nil {
		return nil
	}
	if uint32(want) != sum {
		return fmt.Errorf("yenc: crc32 mismatch: decoded %08x, =yend declares %08x", sum, want)
	}
	return nil
}
