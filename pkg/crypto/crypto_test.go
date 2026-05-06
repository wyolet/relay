package crypto

import (
	"bytes"
	"encoding/base64"
	"testing"
)

var key32 = bytes.Repeat([]byte{0x42}, 32)

func TestRoundTrip(t *testing.T) {
	sizes := []int{0, 1, 256, 4 * 1024, 64 * 1024}
	for _, sz := range sizes {
		plain := make([]byte, sz)
		for i := range plain {
			plain[i] = byte(i)
		}
		ct, nonce, err := Encrypt(key32, plain)
		if err != nil {
			t.Fatalf("Encrypt size=%d: %v", sz, err)
		}
		got, err := Decrypt(key32, ct, nonce)
		if err != nil {
			t.Fatalf("Decrypt size=%d: %v", sz, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("round-trip mismatch size=%d", sz)
		}
	}
}

func TestTamperedCiphertext(t *testing.T) {
	ct, nonce, err := Encrypt(key32, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	ct[0] ^= 0xFF
	_, err = Decrypt(key32, ct, nonce)
	if err == nil {
		t.Fatal("expected error on tampered ciphertext")
	}
}

func TestMismatchedKey(t *testing.T) {
	ct, nonce, err := Encrypt(key32, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	otherKey := bytes.Repeat([]byte{0x99}, 32)
	_, err = Decrypt(otherKey, ct, nonce)
	if err == nil {
		t.Fatal("expected error on mismatched key")
	}
}

func TestNonceUniqueness(t *testing.T) {
	seen := make(map[string]bool, 10000)
	plain := []byte("nonce-test")
	for i := 0; i < 10000; i++ {
		_, nonce, err := Encrypt(key32, plain)
		if err != nil {
			t.Fatal(err)
		}
		k := string(nonce)
		if seen[k] {
			t.Fatalf("duplicate nonce at iteration %d", i)
		}
		seen[k] = true
	}
}

func TestParseMasterKey(t *testing.T) {
	// empty
	if _, err := ParseMasterKey(""); err == nil {
		t.Fatal("expected error for empty string")
	}

	// not base64
	if _, err := ParseMasterKey("!!!notbase64!!!"); err == nil {
		t.Fatal("expected error for non-base64")
	}

	// base64 of 31 bytes — valid base64 but wrong decoded length
	if _, err := ParseMasterKey(base64.StdEncoding.EncodeToString(make([]byte, 31))); err == nil {
		t.Fatal("expected error for 31-byte decoded key")
	}

	// base64 of 33 bytes
	if _, err := ParseMasterKey(base64.StdEncoding.EncodeToString(make([]byte, 33))); err == nil {
		t.Fatal("expected error for 33-byte decoded key")
	}
}

func TestGenerateParseMasterKey(t *testing.T) {
	s, err := GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	k, err := ParseMasterKey(s)
	if err != nil {
		t.Fatalf("GenerateMasterKey output rejected by ParseMasterKey: %v", err)
	}
	if len(k) != 32 {
		t.Fatalf("expected 32 bytes, got %d", len(k))
	}
}

func TestEncryptBadKeyLen(t *testing.T) {
	_, _, err := Encrypt(make([]byte, 16), []byte("test"))
	if err == nil {
		t.Fatal("expected error for 16-byte key")
	}
}

func TestDecryptBadKeyLen(t *testing.T) {
	ct, nonce, _ := Encrypt(key32, []byte("test"))
	_, err := Decrypt(make([]byte, 16), ct, nonce)
	if err == nil {
		t.Fatal("expected error for 16-byte key")
	}
}
