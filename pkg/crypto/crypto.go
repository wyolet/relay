// Package crypto provides AES-GCM-256 encryption primitives and master-key
// helpers for Relay's stored-secret subsystem.
// All functions are pure; the package has no state.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// Encrypt encrypts plaintext with AES-GCM-256 using masterKey.
// masterKey must be exactly 32 bytes.
// A fresh 12-byte nonce is read from crypto/rand on each call.
// Returns ciphertext (with GCM auth tag appended by gcm.Seal) and nonce separately;
// callers store them in distinct DB columns.
func Encrypt(masterKey, plaintext []byte) (ciphertext, nonce []byte, err error) {
	if len(masterKey) != 32 {
		return nil, nil, fmt.Errorf("crypto: masterKey must be 32 bytes, got %d", len(masterKey))
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, nil, fmt.Errorf("crypto: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("crypto: new gcm: %w", err)
	}
	n := make([]byte, gcm.NonceSize()) // 12 bytes
	if _, err := io.ReadFull(rand.Reader, n); err != nil {
		return nil, nil, fmt.Errorf("crypto: read rand: %w", err)
	}
	ct := gcm.Seal(nil, n, plaintext, nil)
	return ct, n, nil
}

// Decrypt decrypts ciphertext (with GCM auth tag) using masterKey and nonce.
// Returns an error if the tag is invalid or inputs are malformed; never panics.
func Decrypt(masterKey, ciphertext, nonce []byte) ([]byte, error) {
	if len(masterKey) != 32 {
		return nil, fmt.Errorf("crypto: masterKey must be 32 bytes, got %d", len(masterKey))
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("crypto: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: new gcm: %w", err)
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("crypto: nonce must be %d bytes, got %d", gcm.NonceSize(), len(nonce))
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: decrypt: %w", err)
	}
	return plain, nil
}

// ParseMasterKey base64-decodes (StdEncoding) raw and validates the result is
// exactly 32 bytes. Returns the raw key bytes or a structured error.
func ParseMasterKey(raw string) ([]byte, error) {
	if raw == "" {
		return nil, errors.New("crypto: RELAY_MASTER_KEY is empty")
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("crypto: base64 decode RELAY_MASTER_KEY: %w", err)
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("crypto: RELAY_MASTER_KEY must decode to 32 bytes, got %d", len(b))
	}
	return b, nil
}

// GenerateMasterKey generates 32 random bytes and returns them as a
// base64.StdEncoding string suitable for use as RELAY_MASTER_KEY.
func GenerateMasterKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("crypto: generate master key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}
