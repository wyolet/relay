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
	// Delete removes the secret at id. Deleting a missing id is not an
	// error (idempotent cleanup).
	Delete(ctx context.Context, id string) error
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

// StoredRow is one stored secret's ciphertext, for rotation enumeration.
type StoredRow struct {
	ID         string
	Ciphertext []byte
	Nonce      []byte
	KeyVersion int32
}

// Rotator is the optional transactional re-encryption seam. The store
// implements the transaction boundary; pkg/secret supplies the crypto via
// the reencrypt callback, so all key material stays in this package.
type Rotator interface {
	// Rotate re-encrypts every stored value within one transaction,
	// stamping newKeyVersion. reencrypt maps old (ct, nonce) → new.
	Rotate(ctx context.Context, newKeyVersion int32,
		reencrypt func(ct, nonce []byte) (newCt, newNonce []byte, err error)) (rotated int, err error)
}

// MaxVersioner optionally reports the highest key_version stored, so the
// resolver can align its in-memory version at boot.
type MaxVersioner interface {
	MaxKeyVersion(ctx context.Context) (int32, error)
}

// RotateResult is the outcome of a successful Rotate.
type RotateResult struct {
	Rotated    int
	NewVersion int32
}

// KeyVersion returns the current in-memory master-key version.
func (s *StoredResolver) KeyVersion() int32 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.keyVersion
}

// LoadKeyVersion aligns the in-memory version to the store's maximum, so
// new secrets are tagged with the generation operators last rotated to.
// No-op when the store doesn't report a max or the table is empty.
func (s *StoredResolver) LoadKeyVersion(ctx context.Context) error {
	mv, ok := s.store.(MaxVersioner)
	if !ok {
		return nil
	}
	v, err := mv.MaxKeyVersion(ctx)
	if err != nil {
		return err
	}
	if v > 0 {
		s.mu.Lock()
		s.keyVersion = v
		s.mu.Unlock()
	}
	return nil
}

// Rotate re-encrypts every stored secret with newKey in a single store
// transaction, then swaps the live master key so subsequent Resolve/Create
// use it without a restart. The caller persists newKey to the deployment's
// RELAY_MASTER_KEY so future boots can decrypt.
func (s *StoredResolver) Rotate(ctx context.Context, newKey []byte) (RotateResult, error) {
	if len(newKey) != 32 {
		return RotateResult{}, fmt.Errorf("secret: newKey must be 32 bytes, got %d", len(newKey))
	}
	rot, ok := s.store.(Rotator)
	if !ok {
		return RotateResult{}, fmt.Errorf("secret: store does not support rotation")
	}
	s.mu.RLock()
	oldKey, oldVer := s.masterKey, s.keyVersion
	s.mu.RUnlock()
	if len(oldKey) == 0 {
		return RotateResult{}, fmt.Errorf("secret: current master key not set")
	}
	newVer := oldVer + 1
	n, err := rot.Rotate(ctx, newVer, func(ct, nonce []byte) ([]byte, []byte, error) {
		plain, err := crypto.Decrypt(oldKey, ct, nonce)
		if err != nil {
			return nil, nil, err
		}
		return crypto.Encrypt(newKey, plain)
	})
	if err != nil {
		return RotateResult{}, err
	}
	s.SetMasterKey(newKey, newVer)
	return RotateResult{Rotated: n, NewVersion: newVer}, nil
}

// Delete removes the stored secret at id (idempotent).
func (s *StoredResolver) Delete(ctx context.Context, id string) error {
	return s.store.Delete(ctx, id)
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
