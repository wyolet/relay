package token

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"slices"
	"sync"
)

// Keys is one generation of a key set.
type Keys struct {
	// Signing mints new tokens. Nil on a replica that only verifies.
	Signing ed25519.PrivateKey
	// Current is the key new tokens are minted under, and the one a token
	// with no kid is checked against. Derived from Signing when that is set.
	Current ed25519.PublicKey
	// Previous are retired keys that still verify but never sign.
	Previous []ed25519.PublicKey
}

// KeyStore is where a KeySet reads its keys. A store shared by every replica
// (a database row, a kv entry, a secret manager) makes one replica's rotation
// reach all of them. An empty Keys with a nil error means no keys: tokens are
// off. An error means the store could not be read, and the KeySet keeps what
// it holds.
type KeyStore interface {
	LoadKeys(ctx context.Context) (Keys, error)
}

// StaticStore is a KeyStore over keys held in this process, such as keys from
// the environment. After Set, Reload the KeySets that read it. The zero value
// holds no keys.
type StaticStore struct {
	mu   sync.RWMutex
	keys Keys
}

// NewStaticStore returns a store holding k.
func NewStaticStore(k Keys) *StaticStore {
	s := &StaticStore{}
	s.Set(k)
	return s
}

// Set replaces the keys the store holds.
func (s *StaticStore) Set(k Keys) {
	k.Previous = slices.Clone(k.Previous)
	s.mu.Lock()
	s.keys = k
	s.mu.Unlock()
}

// LoadKeys returns the keys last Set.
func (s *StaticStore) LoadKeys(context.Context) (Keys, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.keys, nil
}

// keyTable is one installed generation, built once so a lookup is a scan of
// a few entries with no allocation. Replaced wholesale, never mutated.
type keyTable struct {
	keys Keys
	// entries holds the current key first, then the previous ones, each kid once.
	entries []keyEntry
}

type keyEntry struct {
	kid string
	pub ed25519.PublicKey
}

func newKeyTable(k Keys) (*keyTable, error) {
	if k.Signing != nil {
		if len(k.Signing) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("token: signing key is %d bytes, want %d", len(k.Signing), ed25519.PrivateKeySize)
		}
		pub, _ := k.Signing.Public().(ed25519.PublicKey)
		if k.Current != nil && !k.Current.Equal(pub) {
			return nil, fmt.Errorf("token: current key is not the signing key's public half")
		}
		k.Current = pub
	}
	t := &keyTable{keys: k}
	if k.Current != nil {
		if err := t.add(k.Current); err != nil {
			return nil, err
		}
	}
	for _, pub := range k.Previous {
		if err := t.add(pub); err != nil {
			return nil, err
		}
	}
	return t, nil
}

func (t *keyTable) add(pub ed25519.PublicKey) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("token: verification key is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	kid := KeyID(pub)
	if _, ok := t.lookup(kid); !ok {
		t.entries = append(t.entries, keyEntry{kid: kid, pub: pub})
	}
	return nil
}

// lookup resolves a kid. A token with no kid predates kid headers and is
// treated as minted under the current key.
func (t *keyTable) lookup(kid string) (ed25519.PublicKey, bool) {
	if t == nil {
		return nil, false
	}
	if kid == "" {
		return t.keys.Current, t.keys.Current != nil
	}
	for _, e := range t.entries {
		if e.kid == kid {
			return e.pub, true
		}
	}
	return nil, false
}
