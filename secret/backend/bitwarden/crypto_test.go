package bitwarden

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/crypto/pbkdf2"
)

func TestParseCipherString_Type2(t *testing.T) {
	iv := make([]byte, 16)
	ct := make([]byte, 32)
	mac := make([]byte, 32)
	for i := range iv {
		iv[i] = byte(i)
	}
	for i := range ct {
		ct[i] = byte(i + 16)
	}
	for i := range mac {
		mac[i] = byte(i + 48)
	}

	s := "2." + base64.StdEncoding.EncodeToString(iv) + "|" +
		base64.StdEncoding.EncodeToString(ct) + "|" +
		base64.StdEncoding.EncodeToString(mac)

	cs, err := parseCipherString(s)
	if err != nil {
		t.Fatalf("parseCipherString failed: %v", err)
	}
	if cs.typ != 2 {
		t.Errorf("expected type 2, got %d", cs.typ)
	}
	if len(cs.iv) != 16 {
		t.Errorf("expected IV length 16, got %d", len(cs.iv))
	}
	if len(cs.ct) != 32 {
		t.Errorf("expected CT length 32, got %d", len(cs.ct))
	}
	if len(cs.mac) != 32 {
		t.Errorf("expected MAC length 32, got %d", len(cs.mac))
	}
}

func TestParseCipherString_Type0(t *testing.T) {
	iv := make([]byte, 16)
	ct := make([]byte, 32)
	s := "0." + base64.StdEncoding.EncodeToString(iv) + "|" +
		base64.StdEncoding.EncodeToString(ct)

	cs, err := parseCipherString(s)
	if err != nil {
		t.Fatalf("parseCipherString failed: %v", err)
	}
	if cs.typ != 0 {
		t.Errorf("expected type 0, got %d", cs.typ)
	}
	if cs.mac != nil {
		t.Error("expected nil MAC for type 0")
	}
}

func TestParseCipherString_Invalid(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"no type", "abcdef"},
		{"bad type", "x.abc|def|ghi"},
		{"type 2 missing mac", "2.abc|def"},
		{"type 0 extra parts", "0.abc|def|ghi"},
		{"unsupported type", "5.abc|def"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseCipherString(tt.input)
			if err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestPKCS7Unpad(t *testing.T) {
	tests := []struct {
		name    string
		input   []byte
		want    []byte
		wantErr bool
	}{
		{
			name:  "1 byte padding",
			input: []byte{0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48, 0x49, 0x4A, 0x4B, 0x4C, 0x4D, 0x4E, 0x4F, 0x01},
			want:  []byte{0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48, 0x49, 0x4A, 0x4B, 0x4C, 0x4D, 0x4E, 0x4F},
		},
		{
			name:  "full block padding",
			input: append(make([]byte, 0), 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10),
			want:  []byte{},
		},
		{"empty input", []byte{}, nil, true},
		{
			name:    "zero padding byte",
			input:   []byte{0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48, 0x49, 0x4A, 0x4B, 0x4C, 0x4D, 0x4E, 0x4F, 0x00},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pkcs7Unpad(tt.input, 16)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Errorf("length mismatch: got %d, want %d", len(got), len(tt.want))
			}
		})
	}
}

func TestDecrypt_Type2_RoundTrip(t *testing.T) {
	encKey := make([]byte, 32)
	macKey := make([]byte, 32)
	for i := range encKey {
		encKey[i] = byte(i)
	}
	for i := range macKey {
		macKey[i] = byte(i + 32)
	}
	key := symmetricKey{encKey: encKey, macKey: macKey}

	plaintext := []byte("Hello, Vaultwarden!")
	s := encryptType2(t, key, plaintext)

	cs, err := parseCipherString(s)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	result, err := cs.decrypt(key)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}

	if string(result) != "Hello, Vaultwarden!" {
		t.Errorf("got %q, want %q", result, "Hello, Vaultwarden!")
	}
}

func TestDecrypt_MACVerificationFails(t *testing.T) {
	key := symmetricKey{encKey: make([]byte, 32), macKey: make([]byte, 32)}

	iv := make([]byte, 16)
	ct := make([]byte, 16)
	wrongMAC := make([]byte, 32)
	for i := range wrongMAC {
		wrongMAC[i] = 0xFF
	}

	s := "2." + base64.StdEncoding.EncodeToString(iv) + "|" +
		base64.StdEncoding.EncodeToString(ct) + "|" +
		base64.StdEncoding.EncodeToString(wrongMAC)

	cs, err := parseCipherString(s)
	if err != nil {
		t.Fatal(err)
	}

	_, err = cs.decrypt(key)
	if err == nil {
		t.Error("expected MAC verification error")
	}
	if !strings.Contains(err.Error(), "MAC verification failed") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestMakeMasterKey_PBKDF2(t *testing.T) {
	key, err := makeMasterKey("password123", "user@example.com", kdfPBKDF2, 600000, nil, nil)
	if err != nil {
		t.Fatalf("makeMasterKey failed: %v", err)
	}
	if len(key) != 32 {
		t.Errorf("expected 32 bytes, got %d", len(key))
	}

	expected := pbkdf2.Key([]byte("password123"), []byte("user@example.com"), 600000, 32, sha256.New)
	for i := range key {
		if key[i] != expected[i] {
			t.Fatalf("key mismatch at byte %d", i)
		}
	}
}

func TestMakeMasterKey_Argon2id(t *testing.T) {
	mem := 64
	par := 4
	key, err := makeMasterKey("password123", "user@example.com", kdfArgon2id, 3, &mem, &par)
	if err != nil {
		t.Fatalf("makeMasterKey failed: %v", err)
	}
	if len(key) != 32 {
		t.Errorf("expected 32 bytes, got %d", len(key))
	}
}

func TestStretchKey(t *testing.T) {
	masterKey := make([]byte, 32)
	for i := range masterKey {
		masterKey[i] = byte(i)
	}

	key, err := stretchKey(masterKey)
	if err != nil {
		t.Fatalf("stretchKey failed: %v", err)
	}

	if len(key.encKey) != 32 {
		t.Errorf("expected 32-byte enc key, got %d", len(key.encKey))
	}
	if len(key.macKey) != 32 {
		t.Errorf("expected 32-byte mac key, got %d", len(key.macKey))
	}

	same := true
	for i := range key.encKey {
		if key.encKey[i] != key.macKey[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("enc and mac keys should differ")
	}
}

func TestDecryptStr_Empty(t *testing.T) {
	key := symmetricKey{encKey: make([]byte, 32), macKey: make([]byte, 32)}
	result, err := decryptStr("", key)
	if err != nil {
		t.Fatalf("decryptStr empty should not error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

func TestParseCipherString_Type3_RSA_SHA256(t *testing.T) {
	ct := make([]byte, 256)
	for i := range ct {
		ct[i] = byte(i)
	}
	s := "3." + base64.StdEncoding.EncodeToString(ct)

	cs, err := parseCipherString(s)
	if err != nil {
		t.Fatalf("parseCipherString type 3 failed: %v", err)
	}
	if cs.typ != encTypeRsa2048OaepSha256B64 {
		t.Errorf("expected type %d, got %d", encTypeRsa2048OaepSha256B64, cs.typ)
	}
	if len(cs.ct) != 256 {
		t.Errorf("expected CT length 256, got %d", len(cs.ct))
	}
}

func TestDecryptRSA_OAEP_SHA1_RoundTrip(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	orgKeyPlain := make([]byte, 64)
	for i := range orgKeyPlain {
		orgKeyPlain[i] = byte(i)
	}

	ciphertext, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, &privateKey.PublicKey, orgKeyPlain, nil)
	if err != nil {
		t.Fatalf("RSA encrypt: %v", err)
	}

	csStr := "4." + base64.StdEncoding.EncodeToString(ciphertext)
	cs, err := parseCipherString(csStr)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	decrypted, err := cs.decryptRSA(privateKey)
	if err != nil {
		t.Fatalf("decryptRSA: %v", err)
	}

	if len(decrypted) != 64 {
		t.Fatalf("expected 64 bytes, got %d", len(decrypted))
	}
	for i := range orgKeyPlain {
		if decrypted[i] != orgKeyPlain[i] {
			t.Fatalf("mismatch at byte %d", i)
		}
	}
}

func TestDecryptPrivateKey_RoundTrip(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	derBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal PKCS8: %v", err)
	}

	encKey := make([]byte, 32)
	macKey := make([]byte, 32)
	for i := range encKey {
		encKey[i] = byte(i)
	}
	for i := range macKey {
		macKey[i] = byte(i + 32)
	}
	symKey := symmetricKey{encKey: encKey, macKey: macKey}

	csStr := encryptType2(t, symKey, derBytes)

	decryptedKey, err := decryptPrivateKey(csStr, symKey)
	if err != nil {
		t.Fatalf("decryptPrivateKey: %v", err)
	}

	if decryptedKey.N.Cmp(privateKey.N) != 0 {
		t.Error("decrypted private key N mismatch")
	}
	if decryptedKey.D.Cmp(privateKey.D) != 0 {
		t.Error("decrypted private key D mismatch")
	}
}

func TestDecryptOrgKey_RoundTrip(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	orgKeyPlain := make([]byte, 64)
	for i := 0; i < 32; i++ {
		orgKeyPlain[i] = byte(i)
		orgKeyPlain[32+i] = byte(i + 64)
	}

	ciphertext, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, &privateKey.PublicKey, orgKeyPlain, nil)
	if err != nil {
		t.Fatalf("RSA encrypt: %v", err)
	}

	csStr := "4." + base64.StdEncoding.EncodeToString(ciphertext)

	orgKey, err := decryptOrgKey(csStr, privateKey)
	if err != nil {
		t.Fatalf("decryptOrgKey: %v", err)
	}

	for i := 0; i < 32; i++ {
		if orgKey.encKey[i] != byte(i) {
			t.Fatalf("encKey mismatch at byte %d", i)
		}
	}
	for i := 0; i < 32; i++ {
		if orgKey.macKey[i] != byte(i+64) {
			t.Fatalf("macKey mismatch at byte %d", i)
		}
	}
}

func TestDecryptOrgKey_WrongLength(t *testing.T) {
	privateKey, _ := rsa.GenerateKey(rand.Reader, 2048)

	plaintext := make([]byte, 32)
	ct, _ := rsa.EncryptOAEP(sha1.New(), rand.Reader, &privateKey.PublicKey, plaintext, nil)
	csStr := "4." + base64.StdEncoding.EncodeToString(ct)

	_, err := decryptOrgKey(csStr, privateKey)
	if err == nil {
		t.Error("expected error for wrong org key length")
	}
	if !strings.Contains(err.Error(), "unexpected org key length") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDecryptCipher_WithOrgKey(t *testing.T) {
	personalKey := symmetricKey{encKey: make([]byte, 32), macKey: make([]byte, 32)}
	orgKey := symmetricKey{encKey: make([]byte, 32), macKey: make([]byte, 32)}
	for i := range orgKey.encKey {
		orgKey.encKey[i] = byte(i + 100)
	}
	for i := range orgKey.macKey {
		orgKey.macKey[i] = byte(i + 132)
	}

	encryptedName := encryptType2String(t, orgKey, "ORG_SECRET")
	orgID := "org-123"
	cipher := syncCipher{
		ID:             "cipher-1",
		Type:           cipherTypeLogin,
		OrganizationID: &orgID,
		Name:           encryptedName,
	}

	_, err := decryptCipher(cipher, personalKey)
	if err == nil {
		t.Error("expected error when decrypting org cipher with personal key")
	}

	item, err := decryptCipher(cipher, orgKey)
	if err != nil {
		t.Fatalf("decryptCipher with org key failed: %v", err)
	}
	if item.name != "ORG_SECRET" {
		t.Errorf("got name %q, want %q", item.name, "ORG_SECRET")
	}
}

func encryptType2String(t *testing.T, key symmetricKey, plaintext string) string {
	t.Helper()
	return encryptType2(t, key, []byte(plaintext))
}

func encryptType2(t *testing.T, key symmetricKey, plaintext []byte) string {
	t.Helper()

	padLen := aes.BlockSize - (len(plaintext) % aes.BlockSize)
	padded := make([]byte, len(plaintext)+padLen)
	copy(padded, plaintext)
	for i := len(plaintext); i < len(padded); i++ {
		padded[i] = byte(padLen)
	}

	iv := make([]byte, 16)
	for i := range iv {
		iv[i] = byte(i + 100)
	}

	block, err := aes.NewCipher(key.encKey)
	if err != nil {
		t.Fatal(err)
	}
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)

	mac := hmac.New(sha256.New, key.macKey)
	mac.Write(iv)
	mac.Write(ct)
	macBytes := mac.Sum(nil)

	return fmt.Sprintf("2.%s|%s|%s",
		base64.StdEncoding.EncodeToString(iv),
		base64.StdEncoding.EncodeToString(ct),
		base64.StdEncoding.EncodeToString(macBytes),
	)
}
