package bitwarden

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"hash"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
)

var b64 = base64.StdEncoding.EncodeToString

// seq returns n bytes counting up from start.
func seq(start byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = start + byte(i)
	}
	return b
}

func symKey(seed byte) symmetricKey {
	return symmetricKey{encKey: seq(seed, 32), macKey: seq(seed+32, 32)}
}

var testRSAKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

// encryptType0 returns plaintext as an AesCbc256_B64 cipher string (no MAC) under a fixed IV.
func encryptType0(t *testing.T, encKey, plaintext []byte) string {
	t.Helper()
	iv, ct := cbcEncrypt(t, encKey, plaintext)
	return "0." + b64(iv) + "|" + b64(ct)
}

// encryptType2 returns plaintext as an AesCbc256_HmacSha256_B64 cipher string under a fixed IV.
func encryptType2(t *testing.T, key symmetricKey, plaintext []byte) string {
	t.Helper()
	iv, ct := cbcEncrypt(t, key.encKey, plaintext)
	mac := hmac.New(sha256.New, key.macKey)
	mac.Write(iv)
	mac.Write(ct)
	return "2." + b64(iv) + "|" + b64(ct) + "|" + b64(mac.Sum(nil))
}

func encryptType2String(t *testing.T, key symmetricKey, plaintext string) string {
	t.Helper()
	return encryptType2(t, key, []byte(plaintext))
}

func cbcEncrypt(t *testing.T, encKey, plaintext []byte) (iv, ct []byte) {
	t.Helper()
	padLen := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := append(bytes.Clone(plaintext), bytes.Repeat([]byte{byte(padLen)}, padLen)...)
	block, err := aes.NewCipher(encKey)
	must(t, err)
	iv, ct = seq(100, 16), make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)
	return iv, ct
}

func rsaEncrypt(t *testing.T, typ string, h hash.Hash, plaintext []byte) string {
	t.Helper()
	ct, err := rsa.EncryptOAEP(h, rand.Reader, &testRSAKey().PublicKey, plaintext, nil)
	must(t, err)
	return typ + "." + b64(ct)
}

// wantErr reports whether err is exactly want, where "" means no error.
func wantErr(err error, want string) bool {
	if want == "" {
		return err == nil
	}
	return err != nil && err.Error() == want
}

func TestParseCipherString(t *testing.T) {
	iv, ct, mac := seq(0, 16), seq(16, 32), seq(48, 32)
	for _, tc := range []struct {
		name, in string
		want     *cipherString
		wantErr  string
	}{
		{"type 0", "0." + b64(iv) + "|" + b64(ct), &cipherString{typ: 0, iv: iv, ct: ct}, ""},
		{"type 2", "2." + b64(iv) + "|" + b64(ct) + "|" + b64(mac), &cipherString{typ: 2, iv: iv, ct: ct, mac: mac}, ""},
		{"type 3", "3." + b64(ct), &cipherString{typ: 3, ct: ct}, ""},
		{"type 4", "4." + b64(ct), &cipherString{typ: 4, ct: ct}, ""},
		{"empty", "", nil, "bitwarden: empty cipher string"},
		{"no type", "abcdef", nil, "bitwarden: invalid cipher string: missing type separator"},
		{"type is not a number", "x.abc|def|ghi", nil, `bitwarden: invalid encryption type: strconv.Atoi: parsing "x": invalid syntax`},
		{"type 2 without a mac", "2." + b64(iv) + "|" + b64(ct), nil, "bitwarden: AesCbc256_HmacSha256_B64 expects 3 parts, got 2"},
		{"type 0 with a mac", "0." + b64(iv) + "|" + b64(ct) + "|" + b64(mac), nil, "bitwarden: AesCbc256_B64 expects 2 parts, got 3"},
		{"type 1", "1." + b64(iv) + "|" + b64(ct) + "|" + b64(mac), nil, "bitwarden: unsupported encryption type: 1"},
		{"type 5", "5." + b64(ct), nil, "bitwarden: unsupported encryption type: 5"},
		{"type 0 bad iv", "0.!|" + b64(ct), nil, "bitwarden: invalid IV: illegal base64 data at input byte 0"},
		{"type 0 bad ciphertext", "0." + b64(iv) + "|!", nil, "bitwarden: invalid ciphertext: illegal base64 data at input byte 0"},
		{"type 2 bad iv", "2.!|" + b64(ct) + "|" + b64(mac), nil, "bitwarden: invalid IV: illegal base64 data at input byte 0"},
		{"type 2 bad ciphertext", "2." + b64(iv) + "|!|" + b64(mac), nil, "bitwarden: invalid ciphertext: illegal base64 data at input byte 0"},
		{"type 2 bad mac", "2." + b64(iv) + "|" + b64(ct) + "|!", nil, "bitwarden: invalid MAC: illegal base64 data at input byte 0"},
		{"type 4 bad ciphertext", "4.a|b", nil, "bitwarden: invalid RSA ciphertext: illegal base64 data at input byte 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCipherString(tc.in)
			if d := cmp.Diff(tc.want, got, cmp.AllowUnexported(cipherString{})); d != "" || !wantErr(err, tc.wantErr) {
				t.Errorf("parseCipherString: %v, want %q (-want +got):\n%s", err, tc.wantErr, d)
			}
		})
	}
}

func TestCipherString_Decrypt(t *testing.T) {
	key := symKey(0)
	const plain = "Hello, Vaultwarden!"
	parse := func(s string) *cipherString {
		cs, err := parseCipherString(s)
		must(t, err)
		return cs
	}
	with := func(f func(*cipherString)) *cipherString {
		cs := parse(encryptType2String(t, key, plain))
		f(cs)
		return cs
	}
	const macFailed = "bitwarden: MAC verification failed"

	for _, tc := range []struct {
		name    string
		cs      *cipherString
		key     symmetricKey
		want    string
		wantErr string
	}{
		{"type 2", with(func(*cipherString) {}), key, plain, ""},
		{"type 0", parse(encryptType0(t, key.encKey, []byte(plain))), symmetricKey{encKey: key.encKey}, plain, ""},
		{"a whole block of padding", parse(encryptType2(t, key, seq(0, 16))), key, string(seq(0, 16)), ""},
		{"empty plaintext", parse(encryptType2(t, key, nil)), key, "", ""},
		{"mac bit flipped", with(func(cs *cipherString) { cs.mac[0] ^= 1 }), key, "", macFailed},
		{"mac truncated", with(func(cs *cipherString) { cs.mac = cs.mac[:31] }), key, "", macFailed},
		{"no mac", with(func(cs *cipherString) { cs.mac = nil }), key, "", macFailed},
		{"ciphertext bit flipped", with(func(cs *cipherString) { cs.ct[0] ^= 1 }), key, "", macFailed},
		{"last ciphertext block dropped", with(func(cs *cipherString) { cs.ct = cs.ct[:16] }), key, "", macFailed},
		{"iv bit flipped", with(func(cs *cipherString) { cs.iv[0] ^= 1 }), key, "", macFailed},
		{"another mac key", with(func(*cipherString) {}), symmetricKey{encKey: key.encKey, macKey: seq(99, 32)}, "", macFailed},
		{"key without a mac key", with(func(*cipherString) {}), symmetricKey{encKey: key.encKey}, "", "bitwarden: MAC key required for type 2 decryption"},
		{"short iv", with(func(cs *cipherString) { cs.iv = cs.iv[:15] }), key, "", "bitwarden: invalid IV length: 15"},
		{"no iv", parse("4." + b64(seq(0, 32))), key, "", "bitwarden: invalid IV length: 0"},
		{"ciphertext off the block size", with(func(cs *cipherString) { cs.ct = cs.ct[:17] }), key, "", "bitwarden: invalid ciphertext length: 17"},
		{"empty ciphertext", with(func(cs *cipherString) { cs.ct = nil }), key, "", "bitwarden: invalid ciphertext length: 0"},
		{"enc key of the wrong size", parse(encryptType0(t, key.encKey, []byte(plain))), symmetricKey{encKey: seq(0, 7)}, "", "bitwarden: aes cipher: crypto/aes: invalid key size 7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.cs.decrypt(tc.key)
			if string(got) != tc.want || !wantErr(err, tc.wantErr) || (err != nil && got != nil) {
				t.Errorf("decrypt = %q, %v; want %q, %q", got, err, tc.want, tc.wantErr)
			}
		})
	}

	t.Run("type 2 relabelled as type 0", func(t *testing.T) {
		cs := with(func(cs *cipherString) { cs.typ, cs.mac = encTypeAesCbc256B64, nil; cs.iv[0] ^= 1 })
		if got, err := cs.decrypt(key); err == nil {
			t.Errorf("decrypt = %q, want an error: the key carries a MAC key and the value carries no MAC", got)
		}
	})
}

func TestPKCS7Unpad(t *testing.T) {
	block := func(data string, pad ...byte) []byte { return append([]byte(data), pad...) }
	for _, tc := range []struct {
		name    string
		in      []byte
		want    string
		wantErr string
	}{
		{"one byte", block("ABCDEFGHIJKLMNO", 1), "ABCDEFGHIJKLMNO", ""},
		{"three bytes", block("ABCDEFGHIJKLM", 3, 3, 3), "ABCDEFGHIJKLM", ""},
		{"whole block", bytes.Repeat([]byte{16}, 16), "", ""},
		{"second block is all padding", block("ABCDEFGHIJKLMNOP", bytes.Repeat([]byte{16}, 16)...), "ABCDEFGHIJKLMNOP", ""},
		{"empty", nil, "", "bitwarden: empty data"},
		{"not block-aligned", block("ABCDEFGHIJKLMN", 1), "", "bitwarden: data not block-aligned"},
		{"zero pad byte", block("ABCDEFGHIJKLMNO", 0), "", "bitwarden: invalid padding length: 0"},
		{"pad longer than a block", block("ABCDEFGHIJKLMNO", 17), "", "bitwarden: invalid padding length: 17"},
		{"pad bytes disagree", block("ABCDEFGHIJKLM", 3, 2, 3), "", "bitwarden: invalid PKCS7 padding"},
		{"pad runs into the data", block("ABCDEFGHIJKLMN", 3, 3), "", "bitwarden: invalid PKCS7 padding"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pkcs7Unpad(tc.in, 16)
			if string(got) != tc.want || !wantErr(err, tc.wantErr) || (err != nil && got != nil) {
				t.Errorf("pkcs7Unpad = %q, %v; want %q, %q", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestDecryptStr(t *testing.T) {
	key := symKey(0)
	for _, tc := range []struct{ name, in, want, wantErr string }{
		{"empty stays empty", "", "", ""},
		{"value", encryptType2String(t, key, "sk-value"), "sk-value", ""},
		{"not a cipher string", "plaintext", "", "bitwarden: invalid cipher string: missing type separator"},
		{"another key's value", encryptType2String(t, symKey(1), "sk-value"), "", "bitwarden: MAC verification failed"},
	} {
		if got, err := decryptStr(tc.in, key); got != tc.want || !wantErr(err, tc.wantErr) {
			t.Errorf("%s: decryptStr = %q, %v; want %q, %q", tc.name, got, err, tc.want, tc.wantErr)
		}
	}
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
