package crypto

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

var key32 = bytes.Repeat([]byte{0x42}, 32)

// Values sealed before the key ring are AES-256-GCM with a 12-byte nonce and no associated data; this vector was produced by another implementation.
func TestDecryptStoredValue(t *testing.T) {
	nonce := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	ct, err := hex.DecodeString("76b5bcb1d44b9368e42b2c7ca2a9765057e4a67de5bddce1d3a67de236d61d408741")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decrypt(key32, ct, nonce)
	if err != nil || string(got) != "sk-legacy-host-key" {
		t.Fatalf("Decrypt = %q, %v; want sk-legacy-host-key", got, err)
	}
}

func TestEncryptDecrypt(t *testing.T) {
	seenNonces := map[string]bool{}
	for _, size := range []int{0, 1, 256, 64 << 10} {
		plain := bytes.Repeat([]byte("secret-"), size/7+1)[:size]
		ct, nonce, err := Encrypt(key32, plain)
		if err != nil {
			t.Fatalf("Encrypt of %d bytes: %v", size, err)
		}
		// The tag adds 16 bytes; nothing of the plaintext shows through.
		if len(ct) != size+16 || len(nonce) != 12 || (size >= 7 && bytes.Contains(ct, []byte("secret-"))) {
			t.Fatalf("Encrypt of %d bytes: %d ciphertext bytes, %d nonce bytes", size, len(ct), len(nonce))
		}
		if got, err := Decrypt(key32, ct, nonce); err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("Decrypt of %d bytes: %v", size, err)
		}
		again, nonce2, _ := Encrypt(key32, plain)
		if bytes.Equal(again, ct) || seenNonces[string(nonce)] || seenNonces[string(nonce2)] || bytes.Equal(nonce, nonce2) {
			t.Fatalf("Encrypt of %d bytes reused a nonce or repeated a ciphertext", size)
		}
		seenNonces[string(nonce)], seenNonces[string(nonce2)] = true, true
	}
}

func TestDecryptRejections(t *testing.T) {
	ct, nonce, err := Encrypt(key32, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	flip := func(b []byte, i int) []byte {
		out := bytes.Clone(b)
		out[i] ^= 0x01
		return out
	}
	for _, tc := range []struct {
		name          string
		key, ct, nnce []byte
		want          string
	}{
		{"first ciphertext byte changed", key32, flip(ct, 0), nonce, "crypto: decrypt: "},
		{"tag changed", key32, flip(ct, len(ct)-1), nonce, "crypto: decrypt: "},
		{"nonce changed", key32, ct, flip(nonce, 0), "crypto: decrypt: "},
		{"another key", bytes.Repeat([]byte{0x99}, 32), ct, nonce, "crypto: decrypt: "},
		{"ciphertext cut short", key32, ct[:len(ct)-1], nonce, "crypto: decrypt: "},
		{"no ciphertext", key32, nil, nonce, "crypto: decrypt: "},
		{"short nonce", key32, ct, nonce[:11], "crypto: nonce must be 12 bytes, got 11"},
		{"no nonce", key32, ct, nil, "crypto: nonce must be 12 bytes, got 0"},
		{"16-byte key", key32[:16], ct, nonce, "crypto: masterKey must be 32 bytes, got 16"},
		{"no key", nil, ct, nonce, "crypto: masterKey must be 32 bytes, got 0"},
	} {
		got, err := Decrypt(tc.key, tc.ct, tc.nnce)
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) || got != nil {
			t.Errorf("%s: Decrypt = %q, %v; want no plaintext and an error starting %q", tc.name, got, err, tc.want)
		}
	}
	if _, _, err := Encrypt(key32[:16], []byte("x")); err == nil || err.Error() != "crypto: masterKey must be 32 bytes, got 16" {
		t.Errorf("Encrypt with a 16-byte key: err = %v", err)
	}
}

func TestParseMasterKey(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"", "crypto: RELAY_MASTER_KEY is empty"},
		{"!!!notbase64!!!", "crypto: base64 decode RELAY_MASTER_KEY: "},
		{base64.RawStdEncoding.EncodeToString(key32), "crypto: base64 decode RELAY_MASTER_KEY: "}, // padding is required
		{base64.StdEncoding.EncodeToString(key32[:31]), "crypto: RELAY_MASTER_KEY must decode to 32 bytes, got 31"},
		{base64.StdEncoding.EncodeToString(append(bytes.Clone(key32), 0)), "crypto: RELAY_MASTER_KEY must decode to 32 bytes, got 33"},
	} {
		if key, err := ParseMasterKey(tc.raw); err == nil || !strings.HasPrefix(err.Error(), tc.want) || key != nil {
			t.Errorf("ParseMasterKey(%q) = %x, %v; want an error starting %q", tc.raw, key, err, tc.want)
		}
	}
	if key, err := ParseMasterKey(base64.StdEncoding.EncodeToString(key32)); err != nil || !bytes.Equal(key, key32) {
		t.Errorf("ParseMasterKey of a 32-byte key = %x, %v", key, err)
	}

	first, err := GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	second, _ := GenerateMasterKey()
	if key, err := ParseMasterKey(first); err != nil || len(key) != 32 || first == second {
		t.Errorf("GenerateMasterKey = %q then %q; parsed to %d bytes, %v", first, second, len(key), err)
	}
}
