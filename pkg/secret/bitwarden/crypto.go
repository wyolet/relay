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
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	encTypeAesCbc256B64           = 0
	encTypeAesCbc256HmacSha256B64 = 2
	encTypeRsa2048OaepSha256B64   = 3
	encTypeRsa2048OaepSha1B64     = 4
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
