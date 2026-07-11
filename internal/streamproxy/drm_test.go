package streamproxy

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"testing"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// drmPKCS7Pad pads data to a multiple of blockSize using PKCS7.
func drmPKCS7Pad(data []byte, blockSize int) []byte {
	pad := blockSize - len(data)%blockSize
	padded := make([]byte, len(data)+pad)
	copy(padded, data)
	for i := len(data); i < len(padded); i++ {
		padded[i] = byte(pad)
	}
	return padded
}

// drmAESCBCEncrypt encrypts plaintext (already padded) with AES-CBC.
func drmAESCBCEncrypt(key, iv, plaintext []byte) []byte {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	ct := make([]byte, len(plaintext))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, plaintext)
	return ct
}

// drmAESCTRXOR encrypts/decrypts data with AES-CTR using a fresh stream from the given counter.
func drmAESCTRXOR(key, ctr []byte, data []byte) []byte {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	out := make([]byte, len(data))
	cipher.NewCTR(block, ctr).XORKeyStream(out, data)
	return out
}

// ---------------------------------------------------------------------------
// CBC tests
// ---------------------------------------------------------------------------

func TestDrmDecryptCBC_roundtrip(t *testing.T) {
	key := []byte("0123456789abcdef") // 16 bytes
	iv := []byte("fedcba9876543210")  // 16 bytes
	plaintext := []byte("Hello, AES-128-CBC segment data!")

	padded := drmPKCS7Pad(plaintext, aes.BlockSize)
	ciphertext := drmAESCBCEncrypt(key, iv, padded)

	got, err := drmDecryptCBC(key, iv, ciphertext)
	if err != nil {
		t.Fatalf("drmDecryptCBC error: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("drmDecryptCBC: got %q, want %q", got, plaintext)
	}
}

func TestDrmDecryptCBC_viaDispatch(t *testing.T) {
	key := []byte("0123456789abcdef")
	iv := []byte("fedcba9876543210")
	plaintext := []byte("dispatch path test data 12345678")

	padded := drmPKCS7Pad(plaintext, aes.BlockSize)
	ciphertext := drmAESCBCEncrypt(key, iv, padded)

	h := New(Config{PublicURL: "https://ext.example"})
	p := DecryptParams{Method: "AES-128", Key: key, IV: iv}
	got, err := drmDecrypt(h, p, ciphertext)
	if err != nil {
		t.Fatalf("drmDecrypt AES-128 error: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("drmDecrypt AES-128: got %q, want %q", got, plaintext)
	}
}

// TestDrmStripPKCS7_invalid tests PKCS7 padding validation directly using known bad inputs.
func TestDrmStripPKCS7_invalid(t *testing.T) {
	// pad byte 17 > blockSize=16: invalid
	bad1 := make([]byte, 16)
	bad1[15] = 17
	if _, err := drmStripPKCS7(bad1); err == nil {
		t.Fatal("expected error for pad value 17 (> block size)")
	}

	// pad byte 0: invalid (PKCS7 minimum pad is 1)
	bad2 := make([]byte, 16)
	bad2[15] = 0
	if _, err := drmStripPKCS7(bad2); err == nil {
		t.Fatal("expected error for pad value 0")
	}

	// inconsistent: claims 3 bytes padding but bytes are [1, 2, 3], not [3, 3, 3]
	bad3 := []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
		0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x01, 0x02, 0x03}
	if _, err := drmStripPKCS7(bad3); err == nil {
		t.Fatal("expected error for inconsistent padding")
	}

	// valid padding: 16 bytes, pad value = 1 → strips last byte
	good := make([]byte, 16)
	good[14] = 0x42
	good[15] = 0x01 // pad = 1
	out, err := drmStripPKCS7(good)
	if err != nil {
		t.Fatalf("unexpected error for valid pad=1: %v", err)
	}
	if len(out) != 15 || out[14] != 0x42 {
		t.Fatalf("drmStripPKCS7 pad=1: got len=%d out[14]=%02x", len(out), out[14])
	}
}

func TestDrmDecryptCBC_notBlockAligned(t *testing.T) {
	key := []byte("0123456789abcdef")
	iv := []byte("fedcba9876543210")
	_, err := drmDecryptCBC(key, iv, []byte("not 16 bytes"))
	if err == nil {
		t.Fatal("expected error for non-block-aligned input, got nil")
	}
}

func TestDrmDecryptCBC_shortKey(t *testing.T) {
	_, err := drmDecryptCBC([]byte("short"), []byte("fedcba9876543210"), make([]byte, 16))
	if err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestDrmDecrypt_emptyMethod(t *testing.T) {
	h := New(Config{PublicURL: "https://ext.example"})
	data := []byte("passthrough")
	got, err := drmDecrypt(h, DecryptParams{Method: ""}, data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("empty method: data should be unchanged")
	}
}

func TestDrmDecrypt_unknownMethod(t *testing.T) {
	h := New(Config{PublicURL: "https://ext.example"})
	_, err := drmDecrypt(h, DecryptParams{Method: "ROT13"}, []byte("x"))
	if err == nil {
		t.Fatal("expected error for unknown method")
	}
}

// ---------------------------------------------------------------------------
// CTR subsample tests
// ---------------------------------------------------------------------------

func TestDrmDecryptCTRSubsamples_wholeBuffer(t *testing.T) {
	key := []byte("0123456789abcdef") // 16 bytes
	// CENC 8-byte IV padded to 16-byte counter.
	iv8 := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	ctr := make([]byte, 16)
	copy(ctr[:8], iv8) // high 8 bytes = IV, low 8 bytes = 0

	plaintext := []byte("The quick brown fox jumps over the lazy dog!!")
	ciphertext := drmAESCTRXOR(key, ctr, plaintext)

	got, err := drmDecryptCTRSubsamples(key, iv8, ciphertext, nil)
	if err != nil {
		t.Fatalf("drmDecryptCTRSubsamples error: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("CTR whole buffer: got %q, want %q", got, plaintext)
	}
}

func TestDrmDecryptCTRSubsamples_subsampleMode(t *testing.T) {
	key := []byte("0123456789abcdef")
	iv8 := []byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x00, 0x11}
	ctr := make([]byte, 16)
	copy(ctr[:8], iv8)

	// Layout: clearA(10) | encB(32) | clearC(6)
	clearA := bytes.Repeat([]byte("A"), 10)
	plainB := bytes.Repeat([]byte("B"), 32)
	clearC := bytes.Repeat([]byte("C"), 6)

	// Encrypt only encB: keystream starts at counter 0 (we're the first encrypted span).
	encB := drmAESCTRXOR(key, ctr, plainB)

	data := bytes.Join([][]byte{clearA, encB, clearC}, nil)

	subs := []drmSubsample{
		{Clear: len(clearA), Encrypted: len(encB)},
		{Clear: len(clearC), Encrypted: 0},
	}

	got, err := drmDecryptCTRSubsamples(key, iv8, data, subs)
	if err != nil {
		t.Fatalf("CTR subsample error: %v", err)
	}

	// clearA should be unchanged.
	if !bytes.Equal(got[:len(clearA)], clearA) {
		t.Fatalf("CTR subsample: clearA corrupted: %q", got[:len(clearA)])
	}
	// encB should be decrypted to plainB.
	gotB := got[len(clearA) : len(clearA)+len(encB)]
	if !bytes.Equal(gotB, plainB) {
		t.Fatalf("CTR subsample: encB decrypted to %q, want %q", gotB, plainB)
	}
	// clearC should be unchanged.
	gotC := got[len(clearA)+len(encB):]
	if !bytes.Equal(gotC, clearC) {
		t.Fatalf("CTR subsample: clearC corrupted: %q", gotC)
	}
}

func TestDrmDecryptCTRSubsamples_shortKey(t *testing.T) {
	_, err := drmDecryptCTRSubsamples([]byte("short"), make([]byte, 8), []byte("data"), nil)
	if err == nil {
		t.Fatal("expected error for short key")
	}
}

// ---------------------------------------------------------------------------
// Box parser test
// ---------------------------------------------------------------------------

// drmBuildBox serializes a 4-byte size + 4-byte type + payload as an fMP4 box.
func drmBuildBox(boxType string, payload []byte) []byte {
	size := 8 + len(payload)
	b := make([]byte, size)
	binary.BigEndian.PutUint32(b[0:4], uint32(size))
	copy(b[4:8], boxType)
	copy(b[8:], payload)
	return b
}

func TestDrmParseBoxes_minimal(t *testing.T) {
	// Build: ftyp(8) + moof{ traf{ tfhd(8) + trun(8) } } + mdat(8)
	tfhd := drmBuildBox("tfhd", make([]byte, 8)) // version+flags+trackID
	trun := drmBuildBox("trun", make([]byte, 8)) // version+flags+sampleCount
	traf := drmBuildBox("traf", append(tfhd, trun...))
	moof := drmBuildBox("moof", traf)
	ftyp := drmBuildBox("ftyp", []byte("isom\x00\x00\x00\x00"))
	mdat := drmBuildBox("mdat", []byte("payload"))

	seg := bytes.Join([][]byte{ftyp, moof, mdat}, nil)

	boxes, err := drmParseBoxes(seg)
	if err != nil {
		t.Fatalf("drmParseBoxes error: %v", err)
	}

	drmAssertBoxType(t, boxes, "ftyp")
	drmAssertBoxType(t, boxes, "moof")
	drmAssertBoxType(t, boxes, "traf")
	drmAssertBoxType(t, boxes, "tfhd")
	drmAssertBoxType(t, boxes, "trun")
	drmAssertBoxType(t, boxes, "mdat")
}

func drmAssertBoxType(t *testing.T, boxes []drmBox, typ string) {
	t.Helper()
	for _, b := range boxes {
		if b.Type == typ {
			return
		}
	}
	t.Errorf("box type %q not found in parsed boxes", typ)
}

// ---------------------------------------------------------------------------
// CENC fMP4 integration test
// ---------------------------------------------------------------------------

// TestDrmDecryptCENC_fMP4Integration hand-builds a minimal
// moof(traf{tfhd,trun,senc})+mdat segment, AES-CTR-encrypts the sample bytes,
// calls drmDecrypt with method="CENC", and verifies that the full box walk
// (offset math, senc parsing, CTR decryption) recovers the plaintext.
func TestDrmDecryptCENC_fMP4Integration(t *testing.T) {
	key := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	iv8 := []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x11, 0x22}

	plaintext := []byte("hello fMP4 CENC!hello fMP4 CENC!") // 32 bytes

	// Build the 16-byte counter block used by CENC for an 8-byte IV:
	// IV occupies the high 8 bytes; the low 8 bytes (block counter) start at 0.
	ctr16 := make([]byte, 16)
	copy(ctr16[:8], iv8)
	ciphertext := drmAESCTRXOR(key, ctr16, plaintext)

	// --- build tfhd ---
	// version(1) + flags(3)=0x020000 (default-base-is-moof) + track_ID(4)=1
	tfhdPayload := make([]byte, 8)
	tfhdPayload[1] = 0x02 // flags high byte: default-base-is-moof
	binary.BigEndian.PutUint32(tfhdPayload[4:], 1)
	tfhdBox := drmBuildBox("tfhd", tfhdPayload)

	// --- build senc ---
	// version(1) + flags(3)=0 + sample_count(4)=1 + IV(8)
	sencPayload := make([]byte, 4+4+8)
	binary.BigEndian.PutUint32(sencPayload[4:], 1) // sample_count = 1
	copy(sencPayload[8:], iv8)
	sencBox := drmBuildBox("senc", sencPayload)

	// --- build trun (placeholder data_offset; filled after moof size is known) ---
	// version(1) + flags(3)=0x000201 (data-offset-present|sample-size-present)
	// + sample_count(4) + data_offset(4) + sample[0].size(4)
	trunFlags := uint32(0x000201)
	trunPayload := make([]byte, 4+4+4+4) // version+flags + count + data_offset + size
	trunPayload[1] = byte(trunFlags >> 16)
	trunPayload[2] = byte(trunFlags >> 8)
	trunPayload[3] = byte(trunFlags)
	binary.BigEndian.PutUint32(trunPayload[4:], 1) // sample_count
	// data_offset filled below
	binary.BigEndian.PutUint32(trunPayload[12:], uint32(len(ciphertext))) // sample size
	trunBox := drmBuildBox("trun", trunPayload)

	// --- assemble traf and moof to compute sizes ---
	trafPayload := append(append(append([]byte(nil), tfhdBox...), trunBox...), sencBox...)
	trafBox := drmBuildBox("traf", trafPayload)
	moofBox := drmBuildBox("moof", trafBox)

	// data_offset = sizeof(moof) + 8 (mdat box header)
	// This is a signed int32 stored at trunPayload[8:12].
	moofSize := len(moofBox)
	dataOffset := moofSize + 8
	binary.BigEndian.PutUint32(trunPayload[8:], uint32(dataOffset))

	// Rebuild with the correct data_offset.
	trunBox = drmBuildBox("trun", trunPayload)
	trafPayload = append(append(append([]byte(nil), tfhdBox...), trunBox...), sencBox...)
	trafBox = drmBuildBox("traf", trafPayload)
	moofBox = drmBuildBox("moof", trafBox)

	// Verify size is stable (no drift after rebuild).
	if len(moofBox) != moofSize {
		t.Fatalf("moof size changed after rebuild: %d → %d", moofSize, len(moofBox))
	}

	// --- build mdat ---
	mdatBox := drmBuildBox("mdat", ciphertext)

	// --- full segment ---
	segment := append(append([]byte(nil), moofBox...), mdatBox...)

	// Sanity: mdat body should start exactly at dataOffset from the segment start.
	mdatBodyOffset := moofSize + 8
	if mdatBodyOffset != dataOffset {
		t.Fatalf("mdatBodyOffset=%d != dataOffset=%d", mdatBodyOffset, dataOffset)
	}

	// --- decrypt via the full drmDecrypt dispatch path ---
	params := DecryptParams{
		Method: "CENC",
		Key:    key,
		IV:     iv8,
	}
	result, err := drmDecrypt(nil, params, segment)
	if err != nil {
		t.Fatalf("drmDecrypt error: %v", err)
	}

	// The mdat body in the result should be the original plaintext.
	mdatBodyStart := moofSize + 8
	mdatBodyEnd := mdatBodyStart + len(plaintext)
	if mdatBodyEnd > len(result) {
		t.Fatalf("result too short: need %d bytes, got %d", mdatBodyEnd, len(result))
	}
	got := result[mdatBodyStart:mdatBodyEnd]
	if !bytes.Equal(got, plaintext) {
		t.Errorf("decrypted mdat body mismatch:\n  got  %x\n  want %x", got, plaintext)
	}
}

// ---------------------------------------------------------------------------
// aesBlockCache cap tests
// ---------------------------------------------------------------------------

// TestCachedAESBlock_capEnforced feeds cachedAESBlock more distinct 16-byte
// keys than aesBlockMaxEntries and asserts the backing map never grows past
// the cap, then verifies a key cached before the clear-and-repopulate still
// round-trips correctly through the freshly derived cipher.Block.
func TestCachedAESBlock_capEnforced(t *testing.T) {
	firstKey := make([]byte, 16)
	binary.BigEndian.PutUint64(firstKey[8:], 0)
	firstBlock, err := cachedAESBlock(firstKey)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("0123456789ABCDEF")
	ciphertext := make([]byte, 16)
	firstBlock.Encrypt(ciphertext, plaintext)

	for i := 1; i <= aesBlockMaxEntries+50; i++ {
		key := make([]byte, 16)
		binary.BigEndian.PutUint64(key[8:], uint64(i))
		if _, err := cachedAESBlock(key); err != nil {
			t.Fatalf("cachedAESBlock(%d): %v", i, err)
		}

		aesBlockMu.Lock()
		n := len(aesBlockCache)
		aesBlockMu.Unlock()
		if n > aesBlockMaxEntries {
			t.Fatalf("aesBlockCache grew to %d entries, want <= %d", n, aesBlockMaxEntries)
		}
	}

	// firstKey was very likely evicted by the clear(s) above; re-deriving its
	// block must still produce a cipher that round-trips the earlier ciphertext.
	block, err := cachedAESBlock(firstKey)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 16)
	block.Decrypt(got, ciphertext)
	if !bytes.Equal(got, plaintext) {
		t.Errorf("round-trip after cache clear mismatch:\n  got  %x\n  want %x", got, plaintext)
	}
}
