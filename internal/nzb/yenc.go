package nzb

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
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
			continue
		case bytes.HasPrefix(raw, []byte("=yend")):
			// end-of-part marker; flush the buffer and stop decoding.
			return bw.Flush()
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
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	// No =yend found (truncated article): flush whatever was buffered.
	return bw.Flush()
}
