// Package memstore is an in-memory keyring.Store for tests and single-process use.
//
// Besides the Store methods it lets a product's in-memory row store reproduce the database checks a persistent store would make: RequirePrimary runs a write under the store's lock only while the named key is its scope's Primary, and the InUse callback is consulted under the same lock by Destroy, so "no row uses this key, destroy it" is one atomic step.
//
// Out of scope: persistence and use across processes.
package memstore

import (
	"bytes"
	"context"
	"errors"
	"sync"

	"github.com/wyolet/relay/secret/keyring"
)

// Store is an in-memory keyring.Store. The zero value is not usable; call New.
type Store struct {
	mu     sync.Mutex
	keys   map[[16]byte]keyring.DataKey
	target [16]byte
	inUse  func(id [16]byte) bool
}

var _ keyring.Store = (*Store)(nil)

// Option configures a Store.
type Option func(*Store)

// WithInUse sets the check Destroy makes before destroying a key. It runs under the store's lock and must not call back into the Store.
func WithInUse(inUse func(id [16]byte) bool) Option {
	return func(s *Store) { s.inUse = inUse }
}

// New returns an empty Store.
func New(opts ...Option) *Store {
	s := &Store{keys: map[[16]byte]keyring.DataKey{}}
	for _, o := range opts {
		o(s)
	}
	return s
}

// RequirePrimary runs write under the store's lock when id is scope's Primary, and returns keyring.ErrNotPrimary otherwise. write must not call back into the Store.
func (s *Store) RequirePrimary(id [16]byte, scope string, write func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok || k.State != keyring.Primary || k.Scope != scope {
		return keyring.ErrNotPrimary
	}
	return write()
}

// List returns copies of every key; callers may modify them freely.
func (s *Store) List(context.Context) ([]keyring.DataKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]keyring.DataKey, 0, len(s.keys))
	for _, k := range s.keys {
		out = append(out, clone(k))
	}
	return out, nil
}

func (s *Store) Get(_ context.Context, id [16]byte) (keyring.DataKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return keyring.DataKey{}, keyring.ErrUnknownKey
	}
	return clone(k), nil
}

func (s *Store) Primary(_ context.Context, scope string) (keyring.DataKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k, ok := s.primaryLocked(scope); ok {
		return clone(k), nil
	}
	return keyring.DataKey{}, keyring.ErrUnknownKey
}

func (s *Store) TargetKEK(context.Context) ([16]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.target, nil
}

// SetTargetKEK refuses a zero id: unset is only the initial state.
func (s *Store) SetTargetKEK(_ context.Context, id, expected [16]byte) error {
	if id == ([16]byte{}) {
		return errors.New("memstore: zero target KEK id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.target != expected {
		return keyring.ErrConflict
	}
	s.target = id
	return nil
}

// InsertPrimary demotes the previous Primary with DemotedAt = k.CreatedAt.
func (s *Store) InsertPrimary(_ context.Context, k keyring.DataKey, expectedPrimary *[16]byte) error {
	if k.State != keyring.Primary || k.Wrapped == nil {
		return errors.New("memstore: InsertPrimary needs a Primary key with wrapped bytes")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.keys[k.ID]; dup {
		return errors.New("memstore: duplicate key id")
	}
	cur, ok := s.primaryLocked(k.Scope)
	if ok != (expectedPrimary != nil) || (ok && cur.ID != *expectedPrimary) {
		return keyring.ErrConflict
	}
	if ok {
		cur.State = keyring.DecryptOnly
		cur.DemotedAt = k.CreatedAt
		s.keys[cur.ID] = cur
	}
	s.keys[k.ID] = clone(k)
	return nil
}

func (s *Store) UpdateWrapped(_ context.Context, id, fromKEK, toKEK [16]byte, wrapped []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return keyring.ErrUnknownKey
	}
	if k.State == keyring.Destroyed || k.KEKID != fromKEK {
		return keyring.ErrConflict
	}
	k.KEKID = toKEK
	k.Wrapped = bytes.Clone(wrapped)
	s.keys[id] = k
	return nil
}

// Destroy consults the WithInUse check under the store's lock, so a RequirePrimary write cannot slip in between the check and the destroy.
func (s *Store) Destroy(_ context.Context, id [16]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	switch {
	case !ok:
		return keyring.ErrUnknownKey
	case k.State == keyring.Destroyed:
		return nil
	case k.State == keyring.Primary:
		return keyring.ErrIsPrimary
	case s.inUse != nil && s.inUse(id):
		return keyring.ErrInUse
	}
	k.State = keyring.Destroyed
	k.Wrapped = nil
	s.keys[id] = k
	return nil
}

func (s *Store) primaryLocked(scope string) (keyring.DataKey, bool) {
	for _, k := range s.keys {
		if k.Scope == scope && k.State == keyring.Primary {
			return k, true
		}
	}
	return keyring.DataKey{}, false
}

func clone(k keyring.DataKey) keyring.DataKey {
	k.Wrapped = bytes.Clone(k.Wrapped)
	return k
}
