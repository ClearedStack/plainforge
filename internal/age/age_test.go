package age

import (
	"bytes"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

type testVectors struct {
	Poly struct {
		KeyHex string `json:"key_hex"`
		Msg    string `json:"msg"`
		TagHex string `json:"tag_hex"`
	} `json:"rfc8439_poly1305"`
	AEAD struct {
		KeyHex              string `json:"key_hex"`
		NonceHex            string `json:"nonce_hex"`
		AADHex              string `json:"aad_hex"`
		Plaintext           string `json:"plaintext"`
		CiphertextAndTagHex string `json:"ciphertext_and_tag_hex"`
	} `json:"rfc8439_aead"`
	AgeV1 struct {
		RecipientPrivHex string `json:"recipient_priv_hex"`
		RecipientPubHex  string `json:"recipient_pub_hex"`
		EphemeralPrivHex string `json:"ephemeral_priv_hex"`
		FileKeyHex       string `json:"file_key_hex"`
		StreamNonceHex   string `json:"stream_nonce_hex"`
		Plaintext        string `json:"plaintext"`
		ArchiveBase64    string `json:"archive_base64"`
	} `json:"age_v1_deterministic"`
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRFC8439AndAgeVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v testVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}

	// 1. RFC 8439 Section 2.5.2 Poly1305 vector.
	var otk [32]byte
	copy(otk[:], mustHex(t, v.Poly.KeyHex))
	gotTag := poly1305Sum([]byte(v.Poly.Msg), otk)
	if hex.EncodeToString(gotTag[:]) != v.Poly.TagHex {
		t.Fatalf("poly1305 tag = %x, want %s", gotTag, v.Poly.TagHex)
	}

	// 2. RFC 8439 Section 2.8.2 ChaCha20-Poly1305 AEAD vector.
	var key [32]byte
	var nonce [12]byte
	copy(key[:], mustHex(t, v.AEAD.KeyHex))
	copy(nonce[:], mustHex(t, v.AEAD.NonceHex))
	aad := mustHex(t, v.AEAD.AADHex)
	sealed := aeadSeal(key, nonce, []byte(v.AEAD.Plaintext), aad)
	if hex.EncodeToString(sealed) != v.AEAD.CiphertextAndTagHex {
		t.Fatalf("AEAD seal = %x, want %s", sealed, v.AEAD.CiphertextAndTagHex)
	}
	opened, err := aeadOpen(nil, key, nonce, sealed, aad)
	if err != nil || string(opened) != v.AEAD.Plaintext {
		t.Fatalf("AEAD open = %q, %v", opened, err)
	}

	// 3. Independent OpenSSL age-encryption.org/v1 vector.
	recPriv, err := ecdh.X25519().NewPrivateKey(mustHex(t, v.AgeV1.RecipientPrivHex))
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{priv: recPriv}
	rec := id.Recipient()
	if hex.EncodeToString(rec.pub.Bytes()) != v.AgeV1.RecipientPubHex {
		t.Fatalf("recipient pub = %x, want %s", rec.pub.Bytes(), v.AgeV1.RecipientPubHex)
	}

	ephPriv, err := ecdh.X25519().NewPrivateKey(mustHex(t, v.AgeV1.EphemeralPrivHex))
	if err != nil {
		t.Fatal(err)
	}
	var fk [16]byte
	var sn [16]byte
	copy(fk[:], mustHex(t, v.AgeV1.FileKeyHex))
	copy(sn[:], mustHex(t, v.AgeV1.StreamNonceHex))

	var buf bytes.Buffer
	w, err := encryptDeterministic(&buf, rec, fk, ephPriv, sn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(v.AgeV1.Plaintext)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	wantArchive, err := base64.StdEncoding.DecodeString(v.AgeV1.ArchiveBase64)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), wantArchive) {
		t.Fatalf("age v1 output mismatch:\ngot  %x\nwant %x", buf.Bytes(), wantArchive)
	}

	r, err := Decrypt(bytes.NewReader(wantArchive), id)
	if err != nil {
		t.Fatalf("Decrypt vector: %v", err)
	}
	dec, err := io.ReadAll(r)
	if err != nil || string(dec) != v.AgeV1.Plaintext {
		t.Fatalf("Decrypt vector plaintext = %q, %v", dec, err)
	}
}

func TestKeyEncodingAndValidation(t *testing.T) {
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	idStr := id.String()
	recStr := id.Recipient().String()
	if !strings.HasPrefix(idStr, "AGE-SECRET-KEY-1") || !strings.HasPrefix(recStr, "age1") {
		t.Fatalf("unexpected prefixes: %q, %q", idStr, recStr)
	}

	parsedID, err := ParseIdentityFile([]byte("# created: 2026-01-01T00:00:00Z\n# public key: " + recStr + "\n" + idStr + "\n"))
	if err != nil || parsedID.String() != idStr {
		t.Fatalf("ParseIdentityFile: %v, %q", err, parsedID.String())
	}
	parsedRec, err := ParseRecipientFile([]byte("# backup recipient\n\n" + recStr + "\n"))
	if err != nil || parsedRec.String() != recStr {
		t.Fatalf("ParseRecipientFile: %v, %q", err, parsedRec.String())
	}

	// Reject malformed keys: uppercase recipient, lowercase secret key, mixed case, flipped char.
	if _, err := ParseRecipient(strings.ToUpper(recStr)); err == nil {
		t.Fatal("expected uppercase recipient to be rejected")
	}
	if _, err := ParseIdentity(strings.ToLower(idStr)); err == nil {
		t.Fatal("expected lowercase secret key to be rejected")
	}
	if _, err := ParseRecipient(recStr[:len(recStr)-1] + "0"); err == nil {
		t.Fatal("expected checksum mismatch to be rejected")
	}
}

func TestStreamingChunkBoundariesAndTampering(t *testing.T) {
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	rec := id.Recipient()

	sizes := []int{
		0,
		1,
		37,
		chunkPlainSize - 1,
		chunkPlainSize,
		chunkPlainSize + 1,
		2 * chunkPlainSize,
		3*chunkPlainSize + 1234,
	}
	for _, sz := range sizes {
		plain := make([]byte, sz)
		for i := range plain {
			plain[i] = byte((i*31 + 7) & 0xff)
		}
		var cipherBuf bytes.Buffer
		w, err := Encrypt(&cipherBuf, rec)
		if err != nil {
			t.Fatalf("size %d Encrypt: %v", sz, err)
		}
		// Write in uneven slices to exercise internal buffering.
		for off := 0; off < len(plain); {
			step := 4093
			if off+step > len(plain) {
				step = len(plain) - off
			}
			if _, err := w.Write(plain[off : off+step]); err != nil {
				t.Fatalf("size %d Write: %v", sz, err)
			}
			off += step
		}
		if err := w.Close(); err != nil {
			t.Fatalf("size %d Close: %v", sz, err)
		}

		// Read back in small slices.
		rd, err := Decrypt(bytes.NewReader(cipherBuf.Bytes()), id)
		if err != nil {
			t.Fatalf("size %d Decrypt: %v", sz, err)
		}
		var got bytes.Buffer
		small := make([]byte, 1019)
		for {
			n, rerr := rd.Read(small)
			if n > 0 {
				got.Write(small[:n])
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				t.Fatalf("size %d Read: %v", sz, rerr)
			}
		}
		if !bytes.Equal(got.Bytes(), plain) {
			t.Fatalf("size %d round-trip mismatch: got %d bytes, want %d", sz, got.Len(), len(plain))
		}
	}

	// Build a 2-chunk archive and verify tamper/truncation refusals.
	twoChunk := bytes.Repeat([]byte("tamper-check-payload-"), 7000) // > 64 KiB
	var valid bytes.Buffer
	w, err := Encrypt(&valid, rec)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(twoChunk)
	w.Close()
	raw := valid.Bytes()

	// 1. Wrong key is rejected during header unwrap.
	if _, err := Decrypt(bytes.NewReader(raw), otherID); err == nil {
		t.Fatal("expected wrong secret key to fail")
	}

	// 2. Flipped bit in header MAC is rejected before streaming payload.
	badHdr := append([]byte(nil), raw...)
	nlIdx := bytes.Index(badHdr, []byte("\n--- "))
	badHdr[nlIdx+6] ^= 0x01
	if _, err := Decrypt(bytes.NewReader(badHdr), id); err == nil {
		t.Fatal("expected corrupted header MAC to fail")
	}

	// 3. Flipped bit in payload ciphertext is rejected on Read.
	badPayload := append([]byte(nil), raw...)
	badPayload[len(badPayload)-10] ^= 0x80
	rd, err := Decrypt(bytes.NewReader(badPayload), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(rd); err == nil {
		t.Fatal("expected corrupted payload chunk to fail")
	}

	// 4. Truncated stream right after the first 64 KiB chunk is rejected because
	// chunk 0 does not carry the final-chunk nonce flag.
	hdrEnd := bytes.Index(raw, []byte("\n--- "))
	afterHdr := hdrEnd + 1 + bytes.IndexByte(raw[hdrEnd+1:], '\n') + 1
	cutAfterChunk0 := raw[:afterHdr+streamNonceSize+chunkCipherSize]
	rd2, err := Decrypt(bytes.NewReader(cutAfterChunk0), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(rd2); err == nil {
		t.Fatal("expected truncated multi-chunk stream to fail")
	}

	// 5. Trailing garbage appended after the final chunk is rejected.
	withTrailing := append(append([]byte(nil), raw...), 0x42)
	rd3, err := Decrypt(bytes.NewReader(withTrailing), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(rd3); err == nil {
		t.Fatal("expected trailing bytes after final chunk to fail")
	}
}
