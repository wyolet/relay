package secret

import (
	"context"
	"fmt"
	"sync"

	"github.com/wyolet/relay/pkg/crypto"
)

var (
	_ Resolver = (*StoredResolver)(nil)
	_ Writer   = (*StoredResolver)(nil)
)

// Store is the persistence seam for stored-secret ciphertext. Its Postgres
// implementation lives in app/ (keeping pkg/secret vendorable). Ciphertext
// + nonce are AES-GCM outputs; key_version records which master-key
// generation encrypted the row, for rotation.
type Store interface {
	// Get returns the ciphertext, nonce, and key version for id, or an
	// error if absent.
	Get(ctx context.Context, id string) (ciphertext, nonce []byte, keyVersion int32, err error)
	// Put writes (overwriting) the ciphertext for id.
	Put(ctx context.Context, id string, ciphertext, nonce []byte, keyVersion int32) error
}

// StoredResolver decrypts AES-GCM ciphertext from a Store using the master
// key. The key is held behind an RWMutex so rotation (SetMasterKey) can
// swap it on a live process without a restart, mirroring the prior
// hostkey-store behavior.
type StoredResolver struct {
	store Store

	mu         sync.RWMutex
	masterKey  []byte
	keyVersion int32
}

// NewStoredResolver constructs a resolver over store, encrypting new
// secrets at keyVersion with masterKey.
func NewStoredResolver(store Store, masterKey []byte, keyVersion int32) *StoredResolver {
	return &StoredResolver{store: store, masterKey: masterKey, keyVersion: keyVersion}
}

// SetMasterKey swaps the active master key + version (post-rotation).
func (s *StoredResolver) SetMasterKey(masterKey []byte, keyVersion int32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.masterKey = masterKey
	s.keyVersion = keyVersion
}

func (s *StoredResolver) Resolve(ctx context.Context, ref Ref) ([]byte, error) {
	if ref.Kind != KindStored {
		return nil, fmt.Errorf("secret/stored: wrong kind %q", ref.Kind)
	}
	ct, nonce, _, err := s.store.Get(ctx, ref.ID)
	if err != nil {
		return nil, fmt.Errorf("secret/stored: get %q: %w", ref.ID, err)
	}
	s.mu.RLock()
	mk := s.masterKey
	s.mu.RUnlock()
	pt, err := crypto.Decrypt(mk, ct, nonce)
	if err != nil {
		return nil, fmt.Errorf("secret/stored: decrypt %q: %w", ref.ID, err)
	}
	return pt, nil
}

// Create encrypts plaintext under the current master key and persists it
// at id, returning a stored Ref. Overwrites any existing secret at id.
func (s *StoredResolver) Create(ctx context.Context, id string, plaintext []byte) (Ref, error) {
	if id == "" {
		return Ref{}, fmt.Errorf("secret/stored: id is required")
	}
	s.mu.RLock()
	mk, ver := s.masterKey, s.keyVersion
	s.mu.RUnlock()
	ct, nonce, err := crypto.Encrypt(mk, plaintext)
	if err != nil {
		return Ref{}, fmt.Errorf("secret/stored: encrypt %q: %w", id, err)
	}
	if err := s.store.Put(ctx, id, ct, nonce, ver); err != nil {
		return Ref{}, fmt.Errorf("secret/stored: put %q: %w", id, err)
	}
	return Ref{Kind: KindStored, ID: id}, nil
}
