// Package aead encrypts byte strings into a self-describing sealed format and opens them again, binding caller-supplied associated data.
//
// A sealed value carries a magic prefix, a format version, the 16-byte id of the key that sealed it, a random 192-bit nonce and the XAES-256-GCM ciphertext; the header is authenticated together with the caller's associated data. The leading zero byte means no text plaintext (JSON, tokens) is ever mistaken for a sealed value, so IsSealed tells sealed values from plaintext. OpenLegacy reads the older shape: AES-256-GCM with a separate 12-byte nonce and no associated data.
//
// Out of scope: choosing, storing, wrapping or rotating keys. The caller supplies the key and its id; this package never writes the legacy shape.
package aead

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
)

// KeySize is the key length in bytes for Seal, Open, OpenLegacy and ParseKey.
const KeySize = 32

// Sealed format v1:
//
//	offset  size  field
//	0       4     magic 0x00 'W' 'S' 'K'
//	4       1     version 0x01
//	5       16    key id
//	21      24    nonce
//	45      n+16  XAES-256-GCM ciphertext || tag
//
// The GCM associated data is header (bytes 0..20) || caller AAD; the header is fixed-length, so the concatenation is unambiguous.
const (
	version1   = 0x01
	keyIDSize  = 16
	nonceSize  = 24
	tagSize    = 16
	headerSize = 4 + 1 + keyIDSize
	overhead   = headerSize + nonceSize + tagSize

	legacyNonceSize = 12
)

var magic = [4]byte{0x00, 'W', 'S', 'K'}

var (
	// ErrNotSealed reports input that does not start with the sealed-format magic or is too short to be a sealed value.
	ErrNotSealed = errors.New("aead: not a sealed value")
	// ErrUnsupportedVersion reports a sealed value whose format version this package cannot open.
	ErrUnsupportedVersion = errors.New("aead: unsupported sealed format version")
	// ErrAuthFailed reports a wrong key, wrong associated data, or tampered input.
	ErrAuthFailed = errors.New("aead: message authentication failed")
)

// Seal encrypts plaintext under key, records keyID in the header, and binds aad. The result is len(plaintext)+61 bytes.
func Seal(key []byte, keyID [16]byte, plaintext, aad []byte) ([]byte, error) {
	x, err := newSealCipher(key)
	if err != nil {
		return nil, err
	}
	if len(plaintext) > math.MaxInt-overhead {
		return nil, errors.New("aead: plaintext too large")
	}
	out := make([]byte, headerSize+nonceSize, overhead+len(plaintext))
	copy(out, magic[:])
	out[4] = version1
	copy(out[5:headerSize], keyID[:])
	nonce := out[headerSize : headerSize+nonceSize]
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("aead: read nonce: %w", err)
	}
	return x.seal(out, nonce, plaintext, associatedData(out[:headerSize], aad))
}

// Open checks the header and decrypts a value produced by Seal under the same key and aad.
func Open(key, sealed, aad []byte) ([]byte, error) {
	x, err := newSealCipher(key)
	if err != nil {
		return nil, err
	}
	if err := checkHeader(sealed); err != nil {
		return nil, err
	}
	nonce := sealed[headerSize : headerSize+nonceSize]
	plain, err := x.open(nil, nonce, sealed[headerSize+nonceSize:], associatedData(sealed[:headerSize], aad))
	if err != nil {
		return nil, ErrAuthFailed
	}
	return plain, nil
}

// KeyID returns the key id recorded in a sealed value's header. It needs no key and does not authenticate the value.
func KeyID(sealed []byte) ([16]byte, error) {
	var id [16]byte
	if err := checkHeader(sealed); err != nil {
		return id, err
	}
	copy(id[:], sealed[5:headerSize])
	return id, nil
}

// IsSealed reports whether b claims the sealed format: the magic plus a version byte. Version and length are left to Open, so a value from a newer format version fails as ErrUnsupportedVersion instead of being taken for plaintext or the legacy shape.
func IsSealed(b []byte) bool {
	return len(b) > len(magic) && bytes.Equal(b[:len(magic)], magic[:])
}

// OpenLegacy decrypts the legacy shape: AES-256-GCM with a separate 12-byte nonce and no associated data.
func OpenLegacy(key, ciphertext, nonce []byte) ([]byte, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("aead: key must be %d bytes, got %d", KeySize, len(key))
	}
	if len(nonce) != legacyNonceSize {
		return nil, fmt.Errorf("aead: nonce must be %d bytes, got %d", legacyNonceSize, len(nonce))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aead: new cipher: %w", err)
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("aead: new gcm: %w", err)
	}
	plain, err := g.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, ErrAuthFailed
	}
	return plain, nil
}

// ParseKey decodes a standard-base64 key and checks it is KeySize bytes.
func ParseKey(raw string) ([]byte, error) {
	if raw == "" {
		return nil, errors.New("aead: key is empty")
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("aead: base64 decode key: %w", err)
	}
	if len(b) != KeySize {
		return nil, fmt.Errorf("aead: key must decode to %d bytes, got %d", KeySize, len(b))
	}
	return b, nil
}

// GenerateKey returns a random KeySize-byte key, standard-base64 encoded for ParseKey.
func GenerateKey() (string, error) {
	b := make([]byte, KeySize)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("aead: generate key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func newSealCipher(key []byte) (*xaes, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("aead: key must be %d bytes, got %d", KeySize, len(key))
	}
	return newXAES(key)
}

func checkHeader(b []byte) error {
	if len(b) < headerSize || !bytes.Equal(b[:4], magic[:]) {
		return ErrNotSealed
	}
	if b[4] != version1 {
		return ErrUnsupportedVersion
	}
	if len(b) < overhead {
		return ErrNotSealed
	}
	return nil
}

func associatedData(header, aad []byte) []byte {
	ad := make([]byte, 0, len(header)+len(aad))
	ad = append(ad, header...)
	return append(ad, aad...)
}
