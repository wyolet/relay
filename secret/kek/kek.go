// Package kek defines key-encryption keys (KEKs): keys that wrap and unwrap data keys, and the Set of KEKs a process holds.
//
// A KEK has a stable, non-secret 16-byte id that is stored next to everything it wraps, so a wrapped value can be matched to the KEK that opens it. The Set has no primary: which KEK wraps new data keys is decided by the caller (the keyring keeps that choice in its store), never by the order KEKs were configured in.
//
// Out of scope: storing wrapped keys, choosing a KEK, and concrete backends (see kek/local).
package kek

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
)

// KEK wraps and unwraps data keys. The aad passed to Unwrap must equal the one passed to Wrap.
type KEK interface {
	// ID is stable for the life of the key and safe to store and log.
	ID() [16]byte
	Wrap(ctx context.Context, dek, aad []byte) ([]byte, error)
	Unwrap(ctx context.Context, wrapped, aad []byte) ([]byte, error)
}

// LegacyOpener is implemented by KEKs that can also decrypt the legacy shape (AES-256-GCM with a separate nonce and no associated data) directly under the key.
type LegacyOpener interface {
	OpenLegacy(ciphertext, nonce []byte) ([]byte, error)
}

// Set is an immutable collection of KEKs addressed by id.
type Set struct {
	byID map[[16]byte]KEK
	ids  [][16]byte
}

// NewSet builds a Set. It refuses an empty set, a nil KEK and two KEKs with the same id.
func NewSet(keks ...KEK) (*Set, error) {
	if len(keks) == 0 {
		return nil, errors.New("kek: empty set")
	}
	s := &Set{byID: make(map[[16]byte]KEK, len(keks)), ids: make([][16]byte, 0, len(keks))}
	for i, k := range keks {
		if k == nil {
			return nil, fmt.Errorf("kek: KEK %d is nil", i)
		}
		id := k.ID()
		if _, dup := s.byID[id]; dup {
			return nil, fmt.Errorf("kek: duplicate KEK id %s", hex.EncodeToString(id[:]))
		}
		s.byID[id] = k
		s.ids = append(s.ids, id)
	}
	return s, nil
}

// Get returns the KEK with the given id.
func (s *Set) Get(id [16]byte) (KEK, bool) {
	k, ok := s.byID[id]
	return k, ok
}

// IDs returns the ids in the order the KEKs were passed to NewSet.
func (s *Set) IDs() [][16]byte {
	return append([][16]byte(nil), s.ids...)
}
