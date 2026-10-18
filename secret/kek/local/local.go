// Package local is a KEK held as 32 raw key bytes in process memory, typically parsed from an environment variable.
//
// Wrapped data keys use the aead sealed format with the KEK id in the key-id slot and "wyolet-secret-dek" prepended to the caller's associated data, so a wrapped key is bound to this KEK and to whatever the caller binds (the keyring binds the data key's id and scope). The KEK also opens the legacy shape directly under its key.
//
// Out of scope: loading the key bytes from configuration and protecting them in memory.
package local

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/wyolet/relay/secret/aead"
)

// ErrWrongKEK reports a wrapped value whose header names a different KEK.
var ErrWrongKEK = errors.New("local: wrapped value belongs to another KEK")

const (
	idLabel   = "wyolet-secret-kek-id"
	wrapLabel = "wyolet-secret-dek"
)

// KEK is a local key-encryption key. It implements kek.KEK and kek.LegacyOpener.
type KEK struct {
	key []byte
	id  [16]byte
}

// New copies key (aead.KeySize bytes) into a KEK.
func New(key []byte) (*KEK, error) {
	if len(key) != aead.KeySize {
		return nil, fmt.Errorf("local: key must be %d bytes, got %d", aead.KeySize, len(key))
	}
	k := &KEK{key: append([]byte(nil), key...)}
	mac := hmac.New(sha256.New, k.key)
	mac.Write([]byte(idLabel))
	copy(k.id[:], mac.Sum(nil))
	return k, nil
}

// ID is the first 16 bytes of HMAC-SHA256(key, "wyolet-secret-kek-id").
func (k *KEK) ID() [16]byte { return k.id }

// Wrap seals dek under the KEK, binding aad.
func (k *KEK) Wrap(_ context.Context, dek, aad []byte) ([]byte, error) {
	return aead.Seal(k.key, k.id, dek, wrapAAD(aad))
}

// Unwrap checks that wrapped names this KEK, then opens it with aad.
func (k *KEK) Unwrap(_ context.Context, wrapped, aad []byte) ([]byte, error) {
	id, err := aead.KeyID(wrapped)
	if err != nil {
		return nil, err
	}
	if id != k.id {
		return nil, ErrWrongKEK
	}
	return aead.Open(k.key, wrapped, wrapAAD(aad))
}

// OpenLegacy decrypts the legacy shape under the KEK's key.
func (k *KEK) OpenLegacy(ciphertext, nonce []byte) ([]byte, error) {
	return aead.OpenLegacy(k.key, ciphertext, nonce)
}

func wrapAAD(aad []byte) []byte {
	b := make([]byte, 0, len(wrapLabel)+len(aad))
	b = append(b, wrapLabel...)
	return append(b, aad...)
}
