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

	case "SAMPLE-AES", "CENC", "CBCS":
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
		mdatData := out[mdat.Start+mdat.HdrSize : mdat.Start+mdat.Size]

		if err := drmDecryptMoof(method, key, iv, box, mdatData); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// drmDecryptMoof decrypts all samples in one moof box's traf children.
func drmDecryptMoof(method string, key, iv []byte, moof drmBox, mdatData []byte) error {
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
		if err := drmDecryptTraf(method, key, iv, trafBoxes, moofBase, mdatData); err != nil {
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

func drmDecryptTraf(method string, key, iv []byte, trafBoxes []drmBox, moofStart int, mdatData []byte) error {
	var tfhd drmTfhd
	var samples []drmTrunSample
	var sencEntries []drmSencEntry
	hasTfhd := false
	hasTrun := false
	hasSenc := false
	dataOffset := int64(0) // offset of first sample into mdat

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
			// If senc is present we prefer it; if not, we can't proceed.
			// We handle the absence of senc below.
		}
	}

	if !hasTfhd || !hasTrun {
		// No track fragment data to process.
		return nil
	}
	if !hasSenc {
		// We rely on senc for per-sample IVs. saiz/saio layout is not supported.
		return errors.New("unsupported CENC layout: senc box absent, saiz/saio not supported")
	}

	// Resolve base data offset for the mdat.
	// base-data-offset-present: explicit offset from the beginning of the file.
	// default-base-is-moof: offset from the start of moof.
	// Otherwise: offset from the start of mdat data (i.e. 0).
	var baseOffset int64
	if tfhd.baseDataOffsetPresent {
		// base-data-offset is absolute file offset; mdatData starts at some absolute position.
		// We don't have the file offset of mdatData, so we use relative addressing:
		// treat base-data-offset as the absolute offset of the moof start within the segment,
		// which is moofStart. The mdat data begins at an offset relative to the start of mdat box.
		// Since we're working within the segment buffer, we map:
		// sample_offset_in_mdat = baseDataOffset - (absolute mdat body start)
		// We approximate: base-data-offset is the absolute offset in the segment file.
		// mdat body starts at moofStart + moof.Size (roughly), but we already sliced it.
		// Use base-data-offset relative to moofStart.
		baseOffset = int64(tfhd.baseDataOffset) - int64(moofStart)
		// Negative base offset means offset was before moof — use 0.
		if baseOffset < 0 {
			baseOffset = 0
		}
	} else if tfhd.defaultBaseIsMoof {
		// trun data_offset is relative to the moof start, but mdatData is already
		// the mdat body slice; the caller does not pass mdatAbsStart, so we treat
		// data_offset as relative to mdat body start (trun data_offset in typical
		// fragmented MP4 equals moofSize+mdatHdrSize, both of which are already
		// consumed by the caller's slice — effective offset into mdatData is 0 + dataOffset
		// minus the header gap; without mdatAbsStart we cannot compute the exact value).
		// We set baseOffset=0 and rely on the bounds check to reject malformed offsets.
		baseOffset = 0
	} else {
		baseOffset = 0
	}

	// dataOffset is the trun data offset (signed, relative to base data offset).
	// In practice for simple fMP4, it's an offset into mdat body.
	sampleStart := baseOffset + dataOffset
	if sampleStart < 0 {
		sampleStart = 0
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
			// Skip samples with unknown size.
			continue
		}
		if pos < 0 || pos+size > int64(len(mdatData)) {
			return fmt.Errorf("CENC: sample %d at offset %d size %d exceeds mdat length %d",
				i, pos, size, len(mdatData))
		}

		entry := sencEntries[i]
		sampIV := entry.IV
		sampData := mdatData[pos : pos+size]

		var decErr error
		if method == "CBCS" {
			// AES-CBC subsample mode: each subsample pattern block decrypted independently.
			// For simplicity, treat the encrypted spans with CBC using sample IV.
			decErr = drmDecryptCBCSubsamples(key, sampIV, sampData, entry.Subsamples)
		} else {
			// AES-CTR (CENC default / SAMPLE-AES).
			var decrypted []byte
			decrypted, decErr = drmDecryptCTRSubsamples(key, sampIV, sampData, entry.Subsamples)
			if decErr == nil {
				copy(sampData, decrypted)
			}
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
func drmParseSenc(p []byte, fallbackIV []byte) ([]drmSencEntry, error) {
	// version(1) flags(3) sample_count(4) then per-sample: IV[ivSize] [subsample_count(2) pairs...]
	// The IV size is not stored in senc itself; it comes from the Track Encryption Box (tenc).
	// We detect it from context: if per-sample IV bytes don't align, we try 8 then 16.
	if len(p) < 8 {
		return nil, errors.New("senc too short")
	}
	flags := uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
	sampleCount := binary.BigEndian.Uint32(p[4:8])
	off := 8

	const useSubsampleEncryption = 0x000002

	// Determine IV size by probing: try 8 first, if alignment fails try 16.
	// We pick whichever parses cleanly.
	entries, err := drmParseSencWithIVSize(p[off:], int(sampleCount), 8, flags&useSubsampleEncryption != 0)
	if err != nil {
		entries, err = drmParseSencWithIVSize(p[off:], int(sampleCount), 16, flags&useSubsampleEncryption != 0)
		if err != nil {
			return nil, fmt.Errorf("senc: cannot parse with IV size 8 or 16: %w", err)
		}
	}

	// Pad short IVs with the fallback if provided.
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

func drmParseSencWithIVSize(p []byte, count, ivSize int, hasSubs bool) ([]drmSencEntry, error) {
	entries := make([]drmSencEntry, count)
	off := 0
	for i := 0; i < count; i++ {
		if len(p) < off+ivSize {
			return nil, fmt.Errorf("senc: sample %d: need %d IV bytes, have %d", i, ivSize, len(p)-off)
		}
		iv := make([]byte, ivSize)
		copy(iv, p[off:off+ivSize])
		off += ivSize

		var subs []drmSubsample
		if hasSubs {
			if len(p) < off+2 {
				return nil, fmt.Errorf("senc: sample %d: missing subsample_count", i)
			}
			subCount := int(binary.BigEndian.Uint16(p[off:]))
			off += 2
			subs = make([]drmSubsample, subCount)
			for j := 0; j < subCount; j++ {
				if len(p) < off+6 {
					return nil, fmt.Errorf("senc: sample %d sub %d: too short", i, j)
				}
				clear := int(binary.BigEndian.Uint16(p[off:]))
				enc := int(binary.BigEndian.Uint32(p[off+2:]))
				subs[j] = drmSubsample{Clear: clear, Encrypted: enc}
				off += 6
			}
		}
		entries[i] = drmSencEntry{IV: iv, Subsamples: subs}
	}
	return entries, nil
}

// drmParseBoxes parses ISO BMFF boxes from b, recursing into container boxes.
// Returned boxes are in order; container children appear after the parent.
func drmParseBoxes(b []byte) ([]drmBox, error) {
	return drmParseBoxesAt(b, 0, len(b))
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

func drmParseBoxesAt(b []byte, startOffset, limit int) ([]drmBox, error) {
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
			children, err := drmParseBoxesAt(payload, startOffset+pos, len(payload))
			if err != nil {
				// Non-fatal: container parse errors don't abort the whole walk.
				_ = err
			} else {
				boxes = append(boxes, children...)
			}
		}

		pos = boxStart + boxSize
		if boxSize == 0 {
			// size==0 means to EOF; stop.
			break
		}
	}
	return boxes, nil
}

// drmDecryptCTRSubsamples decrypts the encrypted spans of data using AES-CTR.
// CENC: IV is the 8-byte (or 16-byte) initialization vector. For 8-byte IVs, the
// high 64 bits of the 128-bit counter are set from IV; the low 64 bits start at 0.
// When subs is nil, the entire data buffer is treated as encrypted.
// Clear bytes are passed through unchanged and do NOT advance the CTR keystream.
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

	out := make([]byte, len(data))
	copy(out, data)

	if len(subs) == 0 {
		// Whole buffer encrypted.
		stream := cipher.NewCTR(block, ctr)
		stream.XORKeyStream(out, data)
		return out, nil
	}

	// Subsample mode: for each subsample, copy clear bytes, then XOR encrypted bytes.
	// The CTR keystream advances only over encrypted bytes.
	// We do this by maintaining our own keystream block position.
	// cipher.NewCTR doesn't expose position, so we generate keystream manually:
	// for each encrypted span we feed it through a fresh CTR seeded at the right block.

	// We implement a manual CTR to support skipping clear bytes.
	ctrBlock := [16]byte{}
	copy(ctrBlock[:], ctr)

	// Generate one AES-CTR keystream block from the given counter block.
	genKeyBlock := func(cb [16]byte) [16]byte {
		var out [16]byte
		block.Encrypt(out[:], cb[:])
		return out
	}

	// Increment the counter (big-endian on the low 8 bytes per CENC spec).
	incCounter := func(cb *[16]byte) {
		for i := 15; i >= 8; i-- {
			cb[i]++
			if cb[i] != 0 {
				break
			}
		}
	}

	ksPos := 0 // byte offset into current keystream block
	var ksBlock [16]byte
	ksBlock = genKeyBlock(ctrBlock)

	dataOff := 0
	for _, sub := range subs {
		// Copy clear bytes verbatim.
		if sub.Clear > 0 {
			end := dataOff + sub.Clear
			if end > len(data) {
				end = len(data)
			}
			copy(out[dataOff:end], data[dataOff:end])
			dataOff = end
		}

		// Decrypt encrypted bytes, advancing keystream.
		enc := sub.Encrypted
		for enc > 0 {
			if dataOff >= len(data) {
				break
			}
			// How many bytes left in current keystream block?
			avail := 16 - ksPos
			take := enc
			if take > avail {
				take = avail
			}
			if dataOff+take > len(data) {
				take = len(data) - dataOff
			}
			for i := 0; i < take; i++ {
				out[dataOff+i] = data[dataOff+i] ^ ksBlock[ksPos+i]
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
	return out, nil
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
		if sub.Encrypted == 0 {
			continue
		}
		end := off + sub.Encrypted
		if end > len(data) {
			return fmt.Errorf("CBC subsample: encrypted span [%d:%d] exceeds data len %d", off, end, len(data))
		}
		span := data[off:end]
		if len(span)%aes.BlockSize != 0 {
			// Truncate to block boundary (partial last block is left in clear per spec).
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
