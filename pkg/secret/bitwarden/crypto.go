// Package bitwarden resolves KindBitwarden secrets by fetching encrypted
// vault data from Vaultwarden/Bitwarden and decrypting client-side.
package bitwarden

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/pbkdf2"
)

const (
	encTypeAesCbc256B64           = 0
	encTypeAesCbc256HmacSha256B64 = 2
	encTypeRsa2048OaepSha256B64   = 3
	encTypeRsa2048OaepSha1B64     = 4

	kdfPBKDF2   = 0
	kdfArgon2id = 1
)

type symmetricKey struct {
	encKey []byte
	macKey []byte
}

type cipherString struct {
	typ int
	iv  []byte
	ct  []byte
	mac []byte
}

func parseCipherString(s string) (*cipherString, error) {
	if s == "" {
		return nil, errors.New("bitwarden: empty cipher string")
	}

	parts := strings.SplitN(s, ".", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("bitwarden: invalid cipher string: missing type separator")
	}

	encType, err := strconv.Atoi(parts[0])
	if err != nil {
		return nil, fmt.Errorf("bitwarden: invalid encryption type: %w", err)
	}

	cs := &cipherString{typ: encType}
	pieces := strings.Split(parts[1], "|")

	switch encType {
	case encTypeAesCbc256B64:
		if len(pieces) != 2 {
			return nil, fmt.Errorf("bitwarden: AesCbc256_B64 expects 2 parts, got %d", len(pieces))
		}
		if cs.iv, err = base64.StdEncoding.DecodeString(pieces[0]); err != nil {
			return nil, fmt.Errorf("bitwarden: invalid IV: %w", err)
		}
		if cs.ct, err = base64.StdEncoding.DecodeString(pieces[1]); err != nil {
			return nil, fmt.Errorf("bitwarden: invalid ciphertext: %w", err)
		}

	case encTypeAesCbc256HmacSha256B64:
		if len(pieces) != 3 {
			return nil, fmt.Errorf("bitwarden: AesCbc256_HmacSha256_B64 expects 3 parts, got %d", len(pieces))
		}
		if cs.iv, err = base64.StdEncoding.DecodeString(pieces[0]); err != nil {
			return nil, fmt.Errorf("bitwarden: invalid IV: %w", err)
		}
		if cs.ct, err = base64.StdEncoding.DecodeString(pieces[1]); err != nil {
			return nil, fmt.Errorf("bitwarden: invalid ciphertext: %w", err)
		}
		if cs.mac, err = base64.StdEncoding.DecodeString(pieces[2]); err != nil {
			return nil, fmt.Errorf("bitwarden: invalid MAC: %w", err)
		}

	case encTypeRsa2048OaepSha256B64, encTypeRsa2048OaepSha1B64:
		raw := strings.Join(pieces, "|")
		if cs.ct, err = base64.StdEncoding.DecodeString(raw); err != nil {
			return nil, fmt.Errorf("bitwarden: invalid RSA ciphertext: %w", err)
		}

	default:
		return nil, fmt.Errorf("bitwarden: unsupported encryption type: %d", encType)
	}

	return cs, nil
}

func (cs *cipherString) decrypt(key symmetricKey) ([]byte, error) {
	if len(cs.iv) != aes.BlockSize {
		return nil, fmt.Errorf("bitwarden: invalid IV length: %d", len(cs.iv))
	}
	if len(cs.ct) == 0 || len(cs.ct)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("bitwarden: invalid ciphertext length: %d", len(cs.ct))
	}

	if cs.typ == encTypeAesCbc256HmacSha256B64 {
		if len(key.macKey) == 0 {
			return nil, errors.New("bitwarden: MAC key required for type 2 decryption")
		}
		mac := hmac.New(sha256.New, key.macKey)
		mac.Write(cs.iv)
		mac.Write(cs.ct)
		expectedMAC := mac.Sum(nil)
		if !hmac.Equal(expectedMAC, cs.mac) {
			return nil, errors.New("bitwarden: MAC verification failed")
		}
	}

	block, err := aes.NewCipher(key.encKey)
	if err != nil {
		return nil, fmt.Errorf("bitwarden: aes cipher: %w", err)
	}

	plaintext := make([]byte, len(cs.ct))
	mode := cipher.NewCBCDecrypter(block, cs.iv)
	mode.CryptBlocks(plaintext, cs.ct)

	plaintext, err = pkcs7Unpad(plaintext, aes.BlockSize)
	if err != nil {
		return nil, fmt.Errorf("bitwarden: pkcs7 unpad: %w", err)
	}

	return plaintext, nil
}

func decryptStr(s string, key symmetricKey) (string, error) {
	if s == "" {
		return "", nil
	}
	cs, err := parseCipherString(s)
	if err != nil {
		return "", err
	}
	b, err := cs.decrypt(key)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func makeMasterKey(password, email string, kdfType, iterations int, memory, parallelism *int) ([]byte, error) {
	salt := []byte(strings.ToLower(strings.TrimSpace(email)))

	switch kdfType {
	case kdfPBKDF2:
		if iterations < 1 {
			return nil, fmt.Errorf("bitwarden: PBKDF2 iterations must be >= 1, got %d", iterations)
		}
		return pbkdf2.Key([]byte(password), salt, iterations, 32, sha256.New), nil

	case kdfArgon2id:
		mem := 64 * 1024
		par := 4
		if memory != nil {
			mem = *memory * 1024
		}
		if parallelism != nil {
			par = *parallelism
		}
		return argon2.IDKey([]byte(password), salt, uint32(iterations), uint32(mem), uint8(par), 32), nil

	default:
		return nil, fmt.Errorf("bitwarden: unsupported KDF type: %d", kdfType)
	}
}

func stretchKey(masterKey []byte) (symmetricKey, error) {
	encKey := make([]byte, 32)
	r := hkdf.Expand(sha256.New, masterKey, []byte("enc"))
	if _, err := io.ReadFull(r, encKey); err != nil {
		return symmetricKey{}, fmt.Errorf("bitwarden: hkdf expand enc: %w", err)
	}

	macKey := make([]byte, 32)
	r = hkdf.Expand(sha256.New, masterKey, []byte("mac"))
	if _, err := io.ReadFull(r, macKey); err != nil {
		return symmetricKey{}, fmt.Errorf("bitwarden: hkdf expand mac: %w", err)
	}

	return symmetricKey{encKey: encKey, macKey: macKey}, nil
}

func decryptSymmetricKey(encryptedKey string, masterKey []byte) (symmetricKey, error) {
	cs, err := parseCipherString(encryptedKey)
	if err != nil {
		return symmetricKey{}, fmt.Errorf("bitwarden: parse encrypted key: %w", err)
	}

	stretched, err := stretchKey(masterKey)
	if err != nil {
		return symmetricKey{}, fmt.Errorf("bitwarden: stretch key: %w", err)
	}

	decrypted, err := cs.decrypt(stretched)
	if err != nil {
		legacy := symmetricKey{encKey: masterKey}
		decrypted, err = cs.decrypt(legacy)
		if err != nil {
			return symmetricKey{}, fmt.Errorf("bitwarden: decrypt symmetric key: %w", err)
		}
	}

	if len(decrypted) != 64 {
		return symmetricKey{}, fmt.Errorf("bitwarden: unexpected symmetric key length: %d (expected 64)", len(decrypted))
	}

	return symmetricKey{
		encKey: decrypted[:32],
		macKey: decrypted[32:],
	}, nil
}

func (cs *cipherString) decryptRSA(privateKey *rsa.PrivateKey) ([]byte, error) {
	switch cs.typ {
	case encTypeRsa2048OaepSha256B64:
		return rsa.DecryptOAEP(sha256.New(), nil, privateKey, cs.ct, nil)
	case encTypeRsa2048OaepSha1B64:
		return rsa.DecryptOAEP(sha1.New(), nil, privateKey, cs.ct, nil)
	default:
		return nil, fmt.Errorf("bitwarden: not an RSA cipher type: %d", cs.typ)
	}
}

func decryptPrivateKey(encryptedPrivateKey string, symKey symmetricKey) (*rsa.PrivateKey, error) {
	if encryptedPrivateKey == "" {
		return nil, errors.New("bitwarden: encrypted private key is empty")
	}

	cs, err := parseCipherString(encryptedPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("bitwarden: parse private key cipher string: %w", err)
	}

	derBytes, err := cs.decrypt(symKey)
	if err != nil {
		return nil, fmt.Errorf("bitwarden: decrypt private key: %w", err)
	}

	parsed, err := x509.ParsePKCS8PrivateKey(derBytes)
	if err != nil {
		return nil, fmt.Errorf("bitwarden: parse PKCS8 private key: %w", err)
	}

	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("bitwarden: private key is not RSA")
	}

	return rsaKey, nil
}

func decryptOrgKey(encryptedOrgKey string, privateKey *rsa.PrivateKey) (symmetricKey, error) {
	if encryptedOrgKey == "" {
		return symmetricKey{}, errors.New("bitwarden: encrypted org key is empty")
	}

	cs, err := parseCipherString(encryptedOrgKey)
	if err != nil {
		return symmetricKey{}, fmt.Errorf("bitwarden: parse org key cipher string: %w", err)
	}

	decrypted, err := cs.decryptRSA(privateKey)
	if err != nil {
		return symmetricKey{}, fmt.Errorf("bitwarden: RSA decrypt org key: %w", err)
	}

	if len(decrypted) != 64 {
		return symmetricKey{}, fmt.Errorf("bitwarden: unexpected org key length: %d (expected 64)", len(decrypted))
	}

	return symmetricKey{
		encKey: decrypted[:32],
		macKey: decrypted[32:],
	}, nil
}

func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("bitwarden: empty data")
	}
	if len(data)%blockSize != 0 {
		return nil, errors.New("bitwarden: data not block-aligned")
	}

	padLen := int(data[len(data)-1])
	if padLen == 0 || padLen > blockSize {
		return nil, fmt.Errorf("bitwarden: invalid padding length: %d", padLen)
	}

	for i := len(data) - padLen; i < len(data); i++ {
		if data[i] != byte(padLen) {
			return nil, errors.New("bitwarden: invalid PKCS7 padding")
		}
	}

	return data[:len(data)-padLen], nil
}
