// SPDX-FileCopyrightText: 2026 The stremio-server-go Authors
//
// SPDX-License-Identifier: MIT

package streamproxy

import (
	"bytes"
	"crypto/aes"
	"encoding/binary"
	"testing"
)

// drmTestPlain returns n deterministic non-trivial bytes.
func drmTestPlain(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7+3) ^ seed
	}
	return b
}

// drmTestCBCSEncrypt is an independent reference implementation of cbcs
// protection for one protected range: it chains blocks manually (no
// cipher.CBC) so it does not share logic with the code under test.
func drmTestCBCSEncrypt(t *testing.T, key, iv, span []byte, crypt, skip int) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	out := append([]byte(nil), span...)
	full := len(span) / aes.BlockSize
	prev := append([]byte(nil), iv...)
	for blk := 0; blk < full; blk++ {
		encrypted := true
		if crypt != 0 {
			encrypted = blk%(crypt+skip) < crypt
		}
		if !encrypted {
			continue
		}
		in := make([]byte, aes.BlockSize)
		for i := range in {
			in[i] = span[blk*aes.BlockSize+i] ^ prev[i]
		}
		block.Encrypt(out[blk*aes.BlockSize:(blk+1)*aes.BlockSize], in)
		copy(prev, out[blk*aes.BlockSize:(blk+1)*aes.BlockSize])
	}
	return out
}

// drmTestCBCSProtect applies cbcs protection to a sample given its subsample map.
func drmTestCBCSProtect(t *testing.T, key, iv, sample []byte, subs []drmSubsample, crypt, skip int) []byte {
	t.Helper()
	if len(subs) == 0 {
		return drmTestCBCSEncrypt(t, key, iv, sample, crypt, skip)
	}
	out := append([]byte(nil), sample...)
	off := 0
	for _, s := range subs {
		off += s.Clear
		enc := drmTestCBCSEncrypt(t, key, iv, sample[off:off+s.Encrypted], crypt, skip)
		copy(out[off:], enc)
		off += s.Encrypted
	}
	return out
}

func TestDrmDecryptCBCSubsamplesPattern(t *testing.T) {
	key := drmTestPlain(16, 0x11)
	iv := drmTestPlain(16, 0x22)

	cases := []struct {
		name        string
		size        int
		subs        []drmSubsample
		crypt, skip int
	}{
		{"video 1:9 single subsample with partial tail", 20 + 16*25 + 7, []drmSubsample{{Clear: 20, Encrypted: 16*25 + 7}}, 1, 9},
		{"video 1:9 two subsamples (IV resets)", 10 + 16*12 + 6 + 16*3, []drmSubsample{{Clear: 10, Encrypted: 16 * 12}, {Clear: 6, Encrypted: 16 * 3}}, 1, 9},
		{"pattern 2:1", 16 * 11, []drmSubsample{{Clear: 0, Encrypted: 16 * 11}}, 2, 1},
		{"no subsamples, unaligned audio, all blocks", 16*5 + 9, nil, 0, 0},
		{"no subsamples, unaligned, 1:9", 16*23 + 3, nil, 1, 9},
		{"shorter than one block stays clear", 11, nil, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plain := drmTestPlain(tc.size, 0x5a)
			prot := drmTestCBCSProtect(t, key, iv, plain, tc.subs, tc.crypt, tc.skip)
			if tc.size >= 32 && bytes.Equal(prot, plain) {
				t.Fatal("test bug: protection was a no-op")
			}
			buf := append([]byte(nil), prot...)
			if err := drmDecryptCBCSubsamples(key, iv, buf, tc.subs, tc.crypt, tc.skip); err != nil {
				t.Fatalf("decrypt: %v", err)
			}
			if !bytes.Equal(buf, plain) {
				t.Errorf("mismatch\n got  %x\n want %x", buf, plain)
			}
		})
	}

	// Decrypting with the wrong pattern must NOT reproduce the plaintext
	// (guards against silently decrypting every block).
	plain := drmTestPlain(16*25, 0x5a)
	subs := []drmSubsample{{Clear: 0, Encrypted: len(plain)}}
	prot := drmTestCBCSProtect(t, key, iv, plain, subs, 1, 9)
	buf := append([]byte(nil), prot...)
	if err := drmDecryptCBCSubsamples(key, iv, buf, subs, 0, 0); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(buf, plain) {
		t.Error("full-block decrypt unexpectedly recovered 1:9 pattern data")
	}
}

func TestDrmDecryptCBCSubsamplesRejectsNegativePattern(t *testing.T) {
	if err := drmDecryptCBCSubsamples(make([]byte, 16), make([]byte, 16), make([]byte, 32), nil, -1, 0); err == nil {
		t.Error("expected error for negative pattern")
	}
}

func drmTestSubsampleSenc(flags uint32, ivSize int, perSample [][]drmSubsample) []byte {
	p := make([]byte, 8)
	p[1] = byte(flags >> 16)
	p[2] = byte(flags >> 8)
	p[3] = byte(flags)
	binary.BigEndian.PutUint32(p[4:], uint32(len(perSample)))
	for i, subs := range perSample {
		p = append(p, drmTestPlain(ivSize, byte(0xc0+i))...)
		if flags&0x2 == 0 {
			continue
		}
		var n [2]byte
		binary.BigEndian.PutUint16(n[:], uint16(len(subs)))
		p = append(p, n[:]...)
		for _, s := range subs {
			var e [6]byte
			binary.BigEndian.PutUint16(e[0:], uint16(s.Clear))
			binary.BigEndian.PutUint32(e[2:], uint32(s.Encrypted))
			p = append(p, e[:]...)
		}
	}
	return p
}

func TestDrmParseSencConstantIV(t *testing.T) {
	subs := [][]drmSubsample{
		{{Clear: 12, Encrypted: 160}, {Clear: 4, Encrypted: 32}},
		{{Clear: 9, Encrypted: 48}},
	}
	payload := drmTestSubsampleSenc(0x2, 0, subs)
	constIV := drmTestPlain(16, 0x77)

	for _, hint := range []int{-1, 0} {
		entries, err := drmParseSenc(payload, constIV, hint)
		if err != nil {
			t.Fatalf("hint %d: %v", hint, err)
		}
		if len(entries) != 2 {
			t.Fatalf("hint %d: got %d entries", hint, len(entries))
		}
		for i, e := range entries {
			if !bytes.Equal(e.IV, constIV) {
				t.Errorf("hint %d entry %d: IV = %x, want constant IV %x", hint, i, e.IV, constIV)
			}
			if len(e.Subsamples) != len(subs[i]) {
				t.Fatalf("hint %d entry %d: %d subsamples, want %d", hint, i, len(e.Subsamples), len(subs[i]))
			}
			for j := range e.Subsamples {
				if e.Subsamples[j] != subs[i][j] {
					t.Errorf("hint %d entry %d sub %d: %+v want %+v", hint, i, j, e.Subsamples[j], subs[i][j])
				}
			}
		}
	}
}

func TestDrmParseSencPerSampleIVStillDetected(t *testing.T) {
	subs := [][]drmSubsample{{{Clear: 12, Encrypted: 160}}, {{Clear: 9, Encrypted: 48}}}
	for _, ivSize := range []int{8, 16} {
		payload := drmTestSubsampleSenc(0x2, ivSize, subs)
		for _, hint := range []int{-1, ivSize} {
			entries, err := drmParseSenc(payload, nil, hint)
			if err != nil {
				t.Fatalf("iv %d hint %d: %v", ivSize, hint, err)
			}
			if len(entries) != 2 || len(entries[0].IV) != 16 {
				t.Fatalf("iv %d hint %d: bad entries %+v", ivSize, hint, entries)
			}
			if !bytes.Equal(entries[0].IV[:ivSize], drmTestPlain(ivSize, 0xc0)) {
				t.Errorf("iv %d hint %d: IV bytes wrong: %x", ivSize, hint, entries[0].IV)
			}
		}
	}
}

// drmTestBuildTenc builds a tenc box payload (version 1) with a constant IV.
func drmTestBuildTenc(crypt, skip int, constIV []byte) []byte {
	p := make([]byte, 24)
	p[0] = 1
	p[5] = byte(crypt<<4 | skip)
	p[6] = 1 // isProtected
	p[7] = 0 // per-sample IV size
	copy(p[8:], drmTestPlain(16, 0x01))
	p = append(p, byte(len(constIV)))
	return append(p, constIV...)
}

func drmTestBuildMoovWithTenc(tencPayload []byte) []byte {
	frma := drmBuildBox("frma", []byte("avc1"))
	schm := drmBuildBox("schm", append([]byte{0, 0, 0, 0}, []byte("cbcs\x00\x01\x00\x00")...))
	schi := drmBuildBox("schi", drmBuildBox("tenc", tencPayload))
	sinf := drmBuildBox("sinf", append(append(append([]byte(nil), frma...), schm...), schi...))
	encv := drmBuildBox("encv", append(make([]byte, 78), sinf...))
	stsdPayload := append([]byte{0, 0, 0, 0, 0, 0, 0, 1}, encv...)
	stsd := drmBuildBox("stsd", stsdPayload)
	stbl := drmBuildBox("stbl", stsd)
	minf := drmBuildBox("minf", stbl)
	mdia := drmBuildBox("mdia", minf)
	trak := drmBuildBox("trak", mdia)
	return drmBuildBox("moov", trak)
}

// drmTestBuildMediaSegment assembles moof(traf{tfhd,trun,senc}) + mdat.
func drmTestBuildMediaSegment(sencPayload []byte, sizes []int, mdatBody []byte) []byte {
	tfhdPayload := make([]byte, 8)
	tfhdPayload[1] = 0x02 // default-base-is-moof
	binary.BigEndian.PutUint32(tfhdPayload[4:], 1)
	tfhd := drmBuildBox("tfhd", tfhdPayload)
	senc := drmBuildBox("senc", sencPayload)

	build := func(dataOffset uint32) []byte {
		trunPayload := make([]byte, 12+4*len(sizes))
		trunPayload[3] = 0x01 // data-offset-present
		trunPayload[2] = 0x02 // sample-size-present
		binary.BigEndian.PutUint32(trunPayload[4:], uint32(len(sizes)))
		binary.BigEndian.PutUint32(trunPayload[8:], dataOffset)
		for i, s := range sizes {
			binary.BigEndian.PutUint32(trunPayload[12+4*i:], uint32(s))
		}
		trun := drmBuildBox("trun", trunPayload)
		traf := drmBuildBox("traf", append(append(append([]byte(nil), tfhd...), trun...), senc...))
		return drmBuildBox("moof", traf)
	}
	moof := build(0)
	moof = build(uint32(len(moof) + 8))
	return append(moof, drmBuildBox("mdat", mdatBody)...)
}

func drmTestMdatBody(t *testing.T, seg []byte) []byte {
	t.Helper()
	boxes, err := drmParseBoxes(seg)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range boxes {
		if b.Type == "mdat" {
			return seg[b.Start+b.HdrSize : b.Start+b.Size]
		}
	}
	t.Fatal("no mdat in segment")
	return nil
}

// A cbcs segment whose moov carries a tenc with a constant IV and a 2:1
// pattern: senc has no IVs, the decryptor must use the tenc IV and pattern.
func TestDrmDecryptCBCSTencConstantIVAndPattern(t *testing.T) {
	key := drmTestPlain(16, 0x31)
	constIV := drmTestPlain(16, 0x42)
	subs := [][]drmSubsample{
		{{Clear: 14, Encrypted: 16*10 + 5}},
		{{Clear: 3, Encrypted: 16 * 6}},
	}
	plains := [][]byte{drmTestPlain(14+16*10+5, 0x01), drmTestPlain(3+16*6, 0x02)}

	var body, want []byte
	var sizes []int
	for i, p := range plains {
		body = append(body, drmTestCBCSProtect(t, key, constIV, p, subs[i], 2, 1)...)
		want = append(want, p...)
		sizes = append(sizes, len(p))
	}
	seg := append(drmTestBuildMoovWithTenc(drmTestBuildTenc(2, 1, constIV)),
		drmTestBuildMediaSegment(drmTestSubsampleSenc(0x2, 0, subs), sizes, body)...)

	// No iv param: everything must come from the segment itself.
	out, err := drmDecrypt(nil, DecryptParams{Method: "CBCS", Key: key}, seg)
	if err != nil {
		t.Fatalf("drmDecrypt: %v", err)
	}
	if got := drmTestMdatBody(t, out); !bytes.Equal(got, want) {
		t.Errorf("mdat mismatch\n got  %x\n want %x", got, want)
	}
}

// Without a tenc in the segment, the caller's iv is the constant IV and
// subsample samples default to the 1:9 pattern.
func TestDrmDecryptCBCSNoTencUsesQueryIVAndDefaultPattern(t *testing.T) {
	key := drmTestPlain(16, 0x51)
	iv := drmTestPlain(16, 0x62)
	subs := [][]drmSubsample{{{Clear: 20, Encrypted: 16*30 + 11}}}
	plain := drmTestPlain(20+16*30+11, 0x03)
	body := drmTestCBCSProtect(t, key, iv, plain, subs[0], 1, 9)
	seg := drmTestBuildMediaSegment(drmTestSubsampleSenc(0x2, 0, subs), []int{len(plain)}, body)

	out, err := drmDecrypt(nil, DecryptParams{Method: "CBCS", Key: key, IV: iv}, seg)
	if err != nil {
		t.Fatalf("drmDecrypt: %v", err)
	}
	if got := drmTestMdatBody(t, out); !bytes.Equal(got, plain) {
		t.Errorf("mdat mismatch\n got  %x\n want %x", got, plain)
	}

	// Without any IV source the sample must fail loudly, not decrypt with a zero IV.
	if _, err := drmDecrypt(nil, DecryptParams{Method: "CBCS", Key: key}, seg); err == nil {
		t.Error("expected error when neither tenc nor iv supplies the constant IV")
	}
}

func TestDrmParseTenc(t *testing.T) {
	iv := drmTestPlain(16, 0x09)
	tenc, err := drmParseTenc(drmTestBuildTenc(1, 9, iv))
	if err != nil {
		t.Fatal(err)
	}
	if !tenc.found || !tenc.hasPattern || tenc.crypt != 1 || tenc.skip != 9 || tenc.perSampleIVSize != 0 || !bytes.Equal(tenc.constIV, iv) {
		t.Errorf("unexpected tenc: %+v", tenc)
	}
	if _, err := drmParseTenc(make([]byte, 10)); err == nil {
		t.Error("expected error for short tenc")
	}
}
