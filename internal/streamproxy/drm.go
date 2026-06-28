package streamproxy

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

func init() {
	segmentDecryptor = drmDecrypt
}

// drmSubsample describes a single subsample unit inside a CENC-protected sample.
// Clear bytes are copied verbatim; Encrypted bytes are decrypted with AES-CTR.
type drmSubsample struct {
	Clear     int
	Encrypted int
}

// drmBox represents one ISO BMFF box parsed from a byte slice.
type drmBox struct {
	Type    string // 4-char code, or "uuid" with 16-byte usertype suffix
	Start   int    // byte offset of the box start in the outer slice
	HdrSize int    // size of the box header (size field + type + optional largesize + optional uuid)
	Size    int    // total box size in bytes (0 = to EOF, resolved on parse)
	Payload []byte // box body (after header)
}

// drmDecrypt dispatches segment decryption based on p.Method.
func drmDecrypt(h *Handler, p DecryptParams, segment []byte) ([]byte, error) {
	method := strings.TrimSpace(strings.ToUpper(p.Method))
	switch method {
	case "":
		return segment, nil

	case "AES-128":
		if len(p.Key) != 16 {
			return nil, fmt.Errorf("AES-128: key must be 16 bytes, got %d", len(p.Key))
		}
		if len(p.IV) != 16 {
			return nil, fmt.Errorf("AES-128: IV must be 16 bytes, got %d", len(p.IV))
		}
		return drmDecryptCBC(p.Key, p.IV, segment)

	case "SAMPLE-AES":
		return nil, fmt.Errorf("SAMPLE-AES decryption is not supported")

	case "CENC", "CBCS":
		if len(p.Key) != 16 {
			return nil, fmt.Errorf("%s: key must be 16 bytes, got %d", method, len(p.Key))
		}
		return drmDecryptCENC(method, p.Key, p.IV, segment)

	default:
		return nil, fmt.Errorf("unsupported decryption method %q", p.Method)
	}
}

// drmDecryptCBC decrypts a full HLS segment using AES-128-CBC and strips PKCS7 padding.
func drmDecryptCBC(key, iv, data []byte) ([]byte, error) {
	if len(key) != 16 {
		return nil, fmt.Errorf("CBC: key must be 16 bytes, got %d", len(key))
	}
	if len(iv) != 16 {
		return nil, fmt.Errorf("CBC: IV must be 16 bytes, got %d", len(iv))
	}
	if len(data) == 0 {
		return nil, errors.New("CBC: empty segment")
	}
	if len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("CBC: segment length %d is not a multiple of 16", len(data))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("CBC: %w", err)
	}
	out := make([]byte, len(data))
	copy(out, data)
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, out)
	return drmStripPKCS7(out)
}

// drmStripPKCS7 validates and removes PKCS7 padding from a decrypted block.
func drmStripPKCS7(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, errors.New("CBC: empty plaintext after decrypt")
	}
	pad := int(b[len(b)-1])
	if pad == 0 || pad > aes.BlockSize {
		return nil, fmt.Errorf("CBC: invalid PKCS7 padding byte %d", pad)
	}
	if pad > len(b) {
		return nil, fmt.Errorf("CBC: padding length %d exceeds data length %d", pad, len(b))
	}
	// Validate all padding bytes are equal.
	for i := len(b) - pad; i < len(b); i++ {
		if b[i] != byte(pad) {
			return nil, fmt.Errorf("CBC: invalid PKCS7 padding at byte %d", i)
		}
	}
	return b[:len(b)-pad], nil
}

// ---------------------------------------------------------------------------
// CENC fMP4 decryption (AES-CTR for CENC/default; AES-CBC subsample for CBCS)
// ---------------------------------------------------------------------------

// drmDecryptCENC walks an fMP4 segment, locates moof/traf boxes, reads senc/trun/tfhd,
// and decrypts the corresponding mdat ranges in place.
func drmDecryptCENC(method string, key, iv []byte, segment []byte) ([]byte, error) {
	out := make([]byte, len(segment))
	copy(out, segment)

	boxes, err := drmParseBoxes(out)
	if err != nil {
		return nil, fmt.Errorf("CENC: box parse: %w", err)
	}

	// Collect moof boxes; each moof is paired with the immediately-following mdat.
	for i, box := range boxes {
		if box.Type != "moof" {
			continue
		}
		// Find following mdat.
		mdatIdx := -1
		for j := i + 1; j < len(boxes); j++ {
			if boxes[j].Type == "mdat" {
				mdatIdx = j
				break
			}
		}
		if mdatIdx < 0 {
			return nil, errors.New("CENC: moof without following mdat")
		}
		mdat := boxes[mdatIdx]
		mdatBodyStart := mdat.Start + mdat.HdrSize
		mdatData := out[mdatBodyStart : mdat.Start+mdat.Size]

		if err := drmDecryptMoof(method, key, iv, box, mdatData, mdatBodyStart); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// drmDecryptMoof decrypts all samples in one moof box's traf children.
// mdatBodyStart is the absolute byte offset of the first mdat data byte in the segment.
func drmDecryptMoof(method string, key, iv []byte, moof drmBox, mdatData []byte, mdatBodyStart int) error {
	children, err := drmParseBoxes(moof.Payload)
	if err != nil {
		return fmt.Errorf("CENC: moof children: %w", err)
	}

	moofBase := moof.Start

	for _, child := range children {
		if child.Type != "traf" {
			continue
		}
		trafBoxes, err := drmParseBoxes(child.Payload)
		if err != nil {
			return fmt.Errorf("CENC: traf children: %w", err)
		}
		if err := drmDecryptTraf(method, key, iv, trafBoxes, moofBase, mdatData, mdatBodyStart); err != nil {
			return err
		}
	}
	return nil
}

// drmTfhd holds the fields we care about from a Track Fragment Header box.
type drmTfhd struct {
	baseDataOffsetPresent        bool
	defaultBaseIsMoof            bool
	defaultSampleSizePresent     bool
	defaultSampleDurationPresent bool
	defaultSampleFlagsPresent    bool
	baseDataOffset               uint64
	defaultSampleSize            uint32
	defaultSampleDuration        uint32
}

// drmTrunSample holds per-sample values from trun.
type drmTrunSample struct {
	Duration  uint32
	Size      uint32
	Flags     uint32
	CTSOffset int32
}

// drmSencEntry holds per-sample IV and optional subsamples from senc.
type drmSencEntry struct {
	IV         []byte
	Subsamples []drmSubsample
}

func drmDecryptTraf(method string, key, iv []byte, trafBoxes []drmBox, moofStart int, mdatData []byte, mdatBodyStart int) error {
	var tfhd drmTfhd
	var samples []drmTrunSample
	var sencEntries []drmSencEntry
	hasTfhd := false
	hasTrun := false
	hasSenc := false
	dataOffset := int64(0) // trun data-offset; 0 when flag absent

	for _, b := range trafBoxes {
		switch b.Type {
		case "tfhd":
			var err error
			tfhd, err = drmParseTfhd(b.Payload)
			if err != nil {
				return fmt.Errorf("CENC: tfhd: %w", err)
			}
			hasTfhd = true

		case "trun":
			var err error
			var off int32
			samples, off, err = drmParseTrun(b.Payload)
			if err != nil {
				return fmt.Errorf("CENC: trun: %w", err)
			}
			dataOffset = int64(off)
			hasTrun = true

		case "senc":
			var err error
			sencEntries, err = drmParseSenc(b.Payload, iv)
			if err != nil {
				return fmt.Errorf("CENC: senc: %w", err)
			}
			hasSenc = true

		case "saiz", "saio":
			// senc is preferred; if absent, we cannot proceed (handled below).
		}
	}

	if !hasTfhd || !hasTrun {
		return nil
	}
	if !hasSenc {
		return errors.New("unsupported CENC layout: senc box absent, saiz/saio not supported")
	}

	// Resolve the absolute base for sample data per ISO 14496-12 §8.8.8:
	//   base-data-offset-present → explicit absolute file offset.
	//   default-base-is-moof (or implicit for first traf) → start of enclosing moof.
	// trun.data_offset is a signed offset relative to that base.
	// mdatBodyStart is the absolute position of mdatData[0] in the segment buffer.
	var base int64
	if tfhd.baseDataOffsetPresent {
		base = int64(tfhd.baseDataOffset)
	} else {
		// Both default-base-is-moof and the implicit-first-traf rule set the base
		// to the start of the enclosing moof box.
		base = int64(moofStart)
	}

	sampleStart := base + dataOffset - int64(mdatBodyStart)
	if sampleStart < 0 {
		return fmt.Errorf("CENC: computed sample start %d is before mdat body (base=%d, dataOffset=%d, mdatBodyStart=%d)",
			sampleStart, base, dataOffset, mdatBodyStart)
	}

	if len(sencEntries) != len(samples) {
		return fmt.Errorf("CENC: senc entry count %d != trun sample count %d", len(sencEntries), len(samples))
	}

	pos := sampleStart
	for i, samp := range samples {
		size := int64(samp.Size)
		if size == 0 && tfhd.defaultSampleSizePresent {
			size = int64(tfhd.defaultSampleSize)
		}
		if size == 0 {
			continue
		}
		if pos+size > int64(len(mdatData)) {
			return fmt.Errorf("CENC: sample %d at offset %d size %d exceeds mdat length %d",
				i, pos, size, len(mdatData))
		}

		entry := sencEntries[i]
		sampData := mdatData[pos : pos+size]

		var decErr error
		if method == "CBCS" {
			decErr = drmDecryptCBCSubsamples(key, entry.IV, sampData, entry.Subsamples)
		} else {
			// AES-CTR: decrypt in place; drmDecryptCTRSubsamples XORs sampData directly.
			_, decErr = drmDecryptCTRSubsamples(key, entry.IV, sampData, entry.Subsamples)
		}
		if decErr != nil {
			return fmt.Errorf("CENC: sample %d decrypt: %w", i, decErr)
		}

		pos += size
	}
	return nil
}

// drmParseTfhd parses the Track Fragment Header box payload.
func drmParseTfhd(p []byte) (drmTfhd, error) {
	// version(1) + flags(3) = 4 bytes, then track_ID(4).
	if len(p) < 8 {
		return drmTfhd{}, errors.New("tfhd too short")
	}
	version := p[0]
	_ = version
	flags := uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
	off := 4
	// track_ID
	if len(p) < off+4 {
		return drmTfhd{}, errors.New("tfhd: missing track_ID")
	}
	off += 4

	var tfhd drmTfhd
	const (
		tfhdBaseDataOffsetPresent         = 0x000001
		tfhdSampleDescriptionIndexPresent = 0x000002
		tfhdDefaultSampleDurationPresent  = 0x000008
		tfhdDefaultSampleSizePresent      = 0x000010
		tfhdDefaultSampleFlagsPresent     = 0x000020
		tfhdDurationIsEmpty               = 0x010000
		tfhdDefaultBaseIsMoof             = 0x020000
	)

	tfhd.defaultBaseIsMoof = flags&tfhdDefaultBaseIsMoof != 0

	if flags&tfhdBaseDataOffsetPresent != 0 {
		if len(p) < off+8 {
			return drmTfhd{}, errors.New("tfhd: missing base_data_offset")
		}
		tfhd.baseDataOffsetPresent = true
		tfhd.baseDataOffset = binary.BigEndian.Uint64(p[off:])
		off += 8
	}
	if flags&tfhdSampleDescriptionIndexPresent != 0 {
		if len(p) < off+4 {
			return drmTfhd{}, errors.New("tfhd: missing sample_description_index")
		}
		off += 4
	}
	if flags&tfhdDefaultSampleDurationPresent != 0 {
		if len(p) < off+4 {
			return drmTfhd{}, errors.New("tfhd: missing default_sample_duration")
		}
		tfhd.defaultSampleDurationPresent = true
		tfhd.defaultSampleDuration = binary.BigEndian.Uint32(p[off:])
		off += 4
	}
	if flags&tfhdDefaultSampleSizePresent != 0 {
		if len(p) < off+4 {
			return drmTfhd{}, errors.New("tfhd: missing default_sample_size")
		}
		tfhd.defaultSampleSizePresent = true
		tfhd.defaultSampleSize = binary.BigEndian.Uint32(p[off:])
		off += 4
	}
	if flags&tfhdDefaultSampleFlagsPresent != 0 {
		if len(p) < off+4 {
			return drmTfhd{}, errors.New("tfhd: missing default_sample_flags")
		}
		tfhd.defaultSampleFlagsPresent = true
		off += 4
	}
	_ = off
	return tfhd, nil
}

// drmParseTrun parses the Track Run box payload.
// Returns per-sample records and the data offset (0 if flag absent).
func drmParseTrun(p []byte) ([]drmTrunSample, int32, error) {
	// version(1) flags(3) sample_count(4) [data-offset(4)] [first-sample-flags(4)] [per-sample fields]
	if len(p) < 8 {
		return nil, 0, errors.New("trun too short")
	}
	version := p[0]
	flags := uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
	sampleCount := binary.BigEndian.Uint32(p[4:8])
	off := 8

	const (
		trunDataOffsetPresent       = 0x000001
		trunFirstSampleFlagsPresent = 0x000004
		trunSampleDurationPresent   = 0x000100
		trunSampleSizePresent       = 0x000200
		trunSampleFlagsPresent      = 0x000400
		trunSampleCTSOffsetPresent  = 0x000800
	)

	var dataOffset int32
	if flags&trunDataOffsetPresent != 0 {
		if len(p) < off+4 {
			return nil, 0, errors.New("trun: missing data_offset")
		}
		dataOffset = int32(binary.BigEndian.Uint32(p[off:]))
		off += 4
	}
	if flags&trunFirstSampleFlagsPresent != 0 {
		if len(p) < off+4 {
			return nil, 0, errors.New("trun: missing first_sample_flags")
		}
		off += 4
	}

	// Bound sample_count against remaining box bytes to prevent hostile allocations.
	// Compute the minimum bytes consumed per sample for the active flags.
	perSampleBytes := 0
	if flags&trunSampleDurationPresent != 0 {
		perSampleBytes += 4
	}
	if flags&trunSampleSizePresent != 0 {
		perSampleBytes += 4
	}
	if flags&trunSampleFlagsPresent != 0 {
		perSampleBytes += 4
	}
	if flags&trunSampleCTSOffsetPresent != 0 {
		perSampleBytes += 4
	}
	if perSampleBytes > 0 {
		maxCount := uint32((len(p) - off) / perSampleBytes)
		if sampleCount > maxCount {
			return nil, 0, fmt.Errorf("trun: sample_count %d exceeds box bounds (%d bytes remaining, %d bytes/sample)",
				sampleCount, len(p)-off, perSampleBytes)
		}
	} else if sampleCount > uint32(len(p)) {
		return nil, 0, fmt.Errorf("trun: sample_count %d exceeds box size %d", sampleCount, len(p))
	}

	samples := make([]drmTrunSample, sampleCount)
	for i := uint32(0); i < sampleCount; i++ {
		var s drmTrunSample
		if flags&trunSampleDurationPresent != 0 {
			if len(p) < off+4 {
				return nil, 0, fmt.Errorf("trun: sample %d missing duration", i)
			}
			s.Duration = binary.BigEndian.Uint32(p[off:])
			off += 4
		}
		if flags&trunSampleSizePresent != 0 {
			if len(p) < off+4 {
				return nil, 0, fmt.Errorf("trun: sample %d missing size", i)
			}
			s.Size = binary.BigEndian.Uint32(p[off:])
			off += 4
		}
		if flags&trunSampleFlagsPresent != 0 {
			if len(p) < off+4 {
				return nil, 0, fmt.Errorf("trun: sample %d missing flags", i)
			}
			s.Flags = binary.BigEndian.Uint32(p[off:])
			off += 4
		}
		if flags&trunSampleCTSOffsetPresent != 0 {
			if len(p) < off+4 {
				return nil, 0, fmt.Errorf("trun: sample %d missing CTS offset", i)
			}
			if version == 1 {
				s.CTSOffset = int32(binary.BigEndian.Uint32(p[off:]))
			} else {
				s.CTSOffset = int32(binary.BigEndian.Uint32(p[off:]))
			}
			off += 4
		}
		samples[i] = s
	}
	return samples, dataOffset, nil
}

// drmParseSenc parses the Sample Encryption box payload.
// fallbackIV is used when the box does not contain per-sample IVs (IV size 0).
//
// The ISO BMFF senc box does not encode the per-sample IV size; that value is
// carried by the Track Encryption Box (tenc) in the moov hierarchy, which is
// not available at this call site.  We resolve the ambiguity structurally: we
// try both IV sizes (8 and 16) and require that exactly one fully consumes the
// box payload.  When both sizes would consume all bytes (only possible when
// sampleCount is zero) we treat the result as unambiguously empty and return
// it directly.  If neither parses successfully, or both parse and leave no
// leftover bytes for a non-zero sample count, we return an error instead of
// silently guessing.
func drmParseSenc(p []byte, fallbackIV []byte) ([]drmSencEntry, error) {
	// version(1) flags(3) sample_count(4) then per-sample: IV[ivSize] [subsample_count(2) pairs...]
	if len(p) < 8 {
		return nil, errors.New("senc too short")
	}
	flags := uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
	sampleCount := binary.BigEndian.Uint32(p[4:8])
	off := 8

	const useSubsampleEncryption = 0x000002
	hasSubs := flags&useSubsampleEncryption != 0

	// Sanity-bound sampleCount before probing: minimum 8 bytes per sample (shortest IV).
	if sampleCount > uint32(len(p)-off) {
		return nil, fmt.Errorf("senc: sample_count %d exceeds remaining box bytes %d", sampleCount, len(p)-off)
	}

	payload := p[off:]
	boxLen := len(payload)

	entries8, consumed8, err8 := drmParseSencWithIVSize(payload, int(sampleCount), 8, hasSubs)
	ok8 := err8 == nil && consumed8 == boxLen

	entries16, consumed16, err16 := drmParseSencWithIVSize(payload, int(sampleCount), 16, hasSubs)
	ok16 := err16 == nil && consumed16 == boxLen

	var entries []drmSencEntry
	switch {
	case ok8 && !ok16:
		entries = entries8
	case ok16 && !ok8:
		entries = entries16
	case ok8 && ok16:
		// Both IV sizes parse cleanly and consume all bytes.  For a non-zero
		// sample count this is structurally ambiguous (the box payload happens
		// to be consistent with two interpretations); return an error rather
		// than guessing.  When sampleCount is zero both produce an empty list
		// which is equivalent regardless of IV size.
		if sampleCount > 0 {
			return nil, errors.New("senc: IV size ambiguous: both 8-byte and 16-byte layouts exactly consume the box payload")
		}
		entries = entries8
	default:
		// Neither IV size results in a parse that exactly consumes the box payload.
		return nil, fmt.Errorf("senc: IV size ambiguous or corrupt: 8-byte: %w, 16-byte: %w", err8, err16)
	}

	// Pad 8-byte IVs to 16 bytes: CENC uses the IV as the high 64 bits of the
	// 128-bit AES-CTR counter block; the low 64 bits start at zero.
	for i := range entries {
		if len(entries[i].IV) == 8 {
			padded := make([]byte, 16)
			copy(padded, entries[i].IV)
			entries[i].IV = padded
		}
		if len(entries[i].IV) == 0 && len(fallbackIV) > 0 {
			entries[i].IV = fallbackIV
		}
	}
	return entries, nil
}

// drmParseSencWithIVSize attempts to parse the senc box payload p with the
// given per-sample IV size.  It returns the parsed entries, the total number
// of bytes consumed from p, and any structural error.  The caller MUST verify
// that consumed == len(p) to confirm the IV size is unambiguous.
func drmParseSencWithIVSize(p []byte, count, ivSize int, hasSubs bool) ([]drmSencEntry, int, error) {
	// Bound count against box size before allocating: each sample needs at least ivSize bytes.
	minPerSample := ivSize
	if hasSubs {
		minPerSample += 2 // subsample_count field
	}
	if minPerSample > 0 && count > len(p)/minPerSample {
		return nil, 0, fmt.Errorf("senc: sample_count %d exceeds box bounds (%d bytes, min %d bytes/sample)",
			count, len(p), minPerSample)
	}

	entries := make([]drmSencEntry, count)
	off := 0
	for i := 0; i < count; i++ {
		if len(p) < off+ivSize {
			return nil, off, fmt.Errorf("senc: sample %d: need %d IV bytes, have %d", i, ivSize, len(p)-off)
		}
		iv := make([]byte, ivSize)
		copy(iv, p[off:off+ivSize])
		off += ivSize

		var subs []drmSubsample
		if hasSubs {
			if len(p) < off+2 {
				return nil, off, fmt.Errorf("senc: sample %d: missing subsample_count", i)
			}
			subCount := int(binary.BigEndian.Uint16(p[off:]))
			off += 2
			// Bound subCount: each subsample is 6 bytes (2 clear + 4 encrypted).
			if subCount > (len(p)-off)/6 {
				return nil, off, fmt.Errorf("senc: sample %d: subsample_count %d exceeds remaining box bytes %d",
					i, subCount, len(p)-off)
			}
			subs = make([]drmSubsample, subCount)
			for j := 0; j < subCount; j++ {
				if len(p) < off+6 {
					return nil, off, fmt.Errorf("senc: sample %d sub %d: too short", i, j)
				}
				clear := int(binary.BigEndian.Uint16(p[off:]))
				enc := int(binary.BigEndian.Uint32(p[off+2:]))
				subs[j] = drmSubsample{Clear: clear, Encrypted: enc}
				off += 6
			}
		}
		entries[i] = drmSencEntry{IV: iv, Subsamples: subs}
	}
	return entries, off, nil
}

// drmParseBoxes parses ISO BMFF boxes from b, recursing into container boxes.
// Returned boxes are in order; container children appear after the parent.
func drmParseBoxes(b []byte) ([]drmBox, error) {
	return drmParseBoxesAt(b, 0, len(b), 0)
}

// drmContainerTypes lists box types that contain child boxes.
var drmContainerTypes = map[string]bool{
	"moof": true,
	"traf": true,
	"moov": true,
	"trak": true,
	"mdia": true,
	"minf": true,
	"stbl": true,
	"edts": true,
	"dinf": true,
	"udta": true,
}

func drmParseBoxesAt(b []byte, startOffset, limit, depth int) ([]drmBox, error) {
	const maxBoxDepth = 32
	if depth > maxBoxDepth {
		return nil, fmt.Errorf("BMFF box nesting exceeds maximum depth (%d)", maxBoxDepth)
	}
	var boxes []drmBox
	pos := 0
	for pos < len(b) {
		if len(b)-pos < 8 {
			// Fewer than 8 bytes: not a valid box header; stop.
			break
		}
		boxStart := pos
		size32 := binary.BigEndian.Uint32(b[pos:])
		typeBytes := b[pos+4 : pos+8]
		boxType := string(typeBytes)
		pos += 8
		hdrSize := 8

		var boxSize int
		switch size32 {
		case 0:
			// Box extends to end of buffer.
			boxSize = len(b) - boxStart
		case 1:
			// Large size: next 8 bytes are the actual size.
			if len(b) < pos+8 {
				return nil, fmt.Errorf("box at %d: size==1 but no largesize", boxStart)
			}
			large := binary.BigEndian.Uint64(b[pos:])
			pos += 8
			hdrSize += 8
			if large > uint64(len(b)-boxStart) {
				return nil, fmt.Errorf("box at %d: largesize %d exceeds buffer", boxStart, large)
			}
			boxSize = int(large)
		default:
			if int(size32) < 8 {
				return nil, fmt.Errorf("box at %d: size %d < 8", boxStart, size32)
			}
			if boxStart+int(size32) > len(b) {
				return nil, fmt.Errorf("box at %d: size %d exceeds buffer len %d", boxStart, size32, len(b))
			}
			boxSize = int(size32)
		}

		// Handle uuid: 16-byte usertype follows the 4-byte type field.
		if boxType == "uuid" {
			if len(b) < pos+12 {
				return nil, fmt.Errorf("uuid box at %d: too short for usertype", boxStart)
			}
			// The usertype is 16 bytes total; we already consumed 4 as boxType.
			// Skip the remaining 12 bytes of the UUID.
			pos += 12
			hdrSize += 12
		}

		endPos := boxStart + boxSize
		if endPos > len(b) {
			endPos = len(b)
		}

		payload := b[pos:endPos]
		box := drmBox{
			Type:    boxType,
			Start:   startOffset + boxStart,
			HdrSize: hdrSize,
			Size:    boxSize,
			Payload: payload,
		}
		boxes = append(boxes, box)

		// Recurse into known containers.
		if drmContainerTypes[boxType] {
			children, err := drmParseBoxesAt(payload, startOffset+pos, len(payload), depth+1)
			if err != nil {
				return boxes, fmt.Errorf("box %q at %d: child parse error: %w", boxType, box.Start, err)
			}
			boxes = append(boxes, children...)
		}

		pos = boxStart + boxSize
		if boxSize == 0 {
			// size==0 means to EOF; stop.
			break
		}
	}
	return boxes, nil
}

// drmDecryptCTRSubsamples decrypts the encrypted spans of data using AES-CTR, in place.
// CENC: IV is the 8-byte (or 16-byte) initialization vector. For 8-byte IVs the
// high 64 bits of the 128-bit counter are set from IV; the low 64 bits start at 0.
// When subs is nil the entire buffer is treated as encrypted.
// Clear bytes are NOT advanced through the CTR keystream.
// Returns data (same slice) on success so callers can chain; the argument is modified.
func drmDecryptCTRSubsamples(key, iv []byte, data []byte, subs []drmSubsample) ([]byte, error) {
	if len(key) != 16 {
		return nil, fmt.Errorf("CTR: key must be 16 bytes, got %d", len(key))
	}

	// Build a 16-byte counter block: IV in high bytes, zeros in low bytes.
	ctr := make([]byte, 16)
	if len(iv) >= 16 {
		copy(ctr, iv[:16])
	} else if len(iv) == 8 {
		// CENC: IV occupies the first 8 bytes; the block counter (low 8) starts at 0.
		copy(ctr[:8], iv)
	} else if len(iv) > 0 {
		copy(ctr, iv)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("CTR: %w", err)
	}

	if len(subs) == 0 {
		// Whole buffer encrypted: XOR in place (cipher.Stream allows src==dst).
		cipher.NewCTR(block, ctr).XORKeyStream(data, data)
		return data, nil
	}

	// Subsample mode: XOR only the encrypted spans in place; skip clear bytes.
	// The CTR keystream advances solely over encrypted bytes (clear bytes do not
	// consume keystream), so we maintain our own per-block position manually.
	ctrBlock := [16]byte{}
	copy(ctrBlock[:], ctr)

	genKeyBlock := func(cb [16]byte) [16]byte {
		var kb [16]byte
		block.Encrypt(kb[:], cb[:])
		return kb
	}

	// Increment the counter on the low 8 bytes (big-endian) per CENC spec.
	incCounter := func(cb *[16]byte) {
		for i := 15; i >= 8; i-- {
			cb[i]++
			if cb[i] != 0 {
				break
			}
		}
	}

	ksPos := 0 // byte position within the current keystream block
	ksBlock := genKeyBlock(ctrBlock)

	dataOff := 0
	for _, sub := range subs {
		// Advance past clear bytes without consuming keystream.
		if sub.Clear > 0 {
			end := dataOff + sub.Clear
			if end > len(data) {
				end = len(data)
			}
			dataOff = end
		}

		// XOR encrypted bytes in place, advancing the keystream.
		enc := sub.Encrypted
		for enc > 0 {
			if dataOff >= len(data) {
				break
			}
			avail := 16 - ksPos
			take := enc
			if take > avail {
				take = avail
			}
			if dataOff+take > len(data) {
				take = len(data) - dataOff
			}
			for i := 0; i < take; i++ {
				data[dataOff+i] ^= ksBlock[ksPos+i]
			}
			dataOff += take
			ksPos += take
			enc -= take
			if ksPos == 16 {
				incCounter(&ctrBlock)
				ksBlock = genKeyBlock(ctrBlock)
				ksPos = 0
			}
		}
	}
	return data, nil
}

// drmDecryptCBCSubsamples decrypts only the encrypted subsample spans with AES-CBC.
// Used for the CBCS protection scheme (pattern-based CBC).
func drmDecryptCBCSubsamples(key, iv []byte, data []byte, subs []drmSubsample) error {
	if len(key) != 16 {
		return fmt.Errorf("CBC subsample: key must be 16 bytes, got %d", len(key))
	}
	if len(iv) != 16 {
		// Pad or reject.
		if len(iv) < 16 {
			padded := make([]byte, 16)
			copy(padded, iv)
			iv = padded
		} else {
			iv = iv[:16]
		}
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return fmt.Errorf("CBC subsample: %w", err)
	}

	if len(subs) == 0 {
		if len(data)%aes.BlockSize != 0 {
			return fmt.Errorf("CBC subsample: data length %d not block-aligned", len(data))
		}
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(data, data)
		return nil
	}

	off := 0
	for _, sub := range subs {
		off += sub.Clear
		if off > len(data) {
			return fmt.Errorf("CBC subsample: clear span exceeds data len %d", len(data))
		}
		if sub.Encrypted == 0 {
			continue
		}
		end := off + sub.Encrypted
		if end > len(data) {
			return fmt.Errorf("CBC subsample: encrypted span [%d:%d] exceeds data len %d", off, end, len(data))
		}
		span := data[off:end]
		if len(span)%aes.BlockSize != 0 {
			// Truncate to block boundary (partial last block left in clear per spec).
			aligned := len(span) &^ (aes.BlockSize - 1)
			span = span[:aligned]
		}
		if len(span) > 0 {
			cipher.NewCBCDecrypter(block, iv).CryptBlocks(span, span)
		}
		off = end
	}
	return nil
}
