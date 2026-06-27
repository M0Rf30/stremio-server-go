package nzb

import (
	"bufio"
	"fmt"
	"io"
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
func DecodeYenc(r io.Reader, w io.Writer) error {
	const maxLine = 256 * 1024
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, maxLine), maxLine)

	inBody := false
	for scanner.Scan() {
		line := scanner.Text()

		switch {
		case strings.HasPrefix(line, "=ybegin"):
			inBody = true
			continue
		case strings.HasPrefix(line, "=ypart"):
			// multi-part header: still inside the data section
			continue
		case strings.HasPrefix(line, "=yend"):
			// end-of-part marker; stop decoding
			return nil
		}

		if !inBody {
			continue
		}

		raw := []byte(line)
		buf := make([]byte, 0, len(raw))
		for i := 0; i < len(raw); i++ {
			b := raw[i]
			if b == '=' {
				i++
				if i >= len(raw) {
					return fmt.Errorf("yenc: escape character at end of line")
				}
				// escaped byte: subtract 64 + 42 (wraps as uint8)
				buf = append(buf, raw[i]-64-42)
			} else {
				// plain byte: subtract 42 (wraps as uint8)
				buf = append(buf, b-42)
			}
		}

		if _, err := w.Write(buf); err != nil {
			return err
		}
	}
	return scanner.Err()
}
