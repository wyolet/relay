// Package keyring encrypts values under per-scope data keys (DEKs) that are wrapped by key-encryption keys (KEKs) and kept in a product-supplied Store.
//
// Each scope (an opaque string the product picks; "" is valid) has one Primary DEK that encrypts; older DEKs stay DecryptOnly until destroyed, and Decrypt reports values under them as stale so the product can rewrite them. Sealed values name their DEK, so a Ring that has not seen a DEK yet, or holds it locked, reloads it from the Store on first sight; writes are expected to be checked against key state by the product's store (ErrNotPrimary), so correctness never depends on change notifications reaching every process. Unwrapped DEKs are cached by id for the life of the Ring and never unwrapped twice, so processes holding different KEK sets can run side by side during a KEK change.
//
// Out of scope: storage (the product implements Store; memstore is an in-memory one), change notification transport, rewriting stale rows, and logging. The Ring starts no goroutines; the product runs Run for the periodic reload.
package keyring

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// State is a data key's lifecycle state. It only moves forward: Primary, DecryptOnly, Destroyed.
type State string

const (
	// Primary is the one key per scope that encrypts.
	Primary State = "primary"
	// DecryptOnly keys still decrypt; values under them are stale.
	DecryptOnly State = "decrypt"
	// Destroyed keys keep a tombstone (id, scope, state) without wrapped bytes.
	Destroyed State = "destroyed"
)

// DataKey is a stored data key. Wrapped is the DEK sealed under KEKID and is nil once Destroyed.
type DataKey struct {
	ID        [16]byte
	Scope     string
	State     State
	KEKID     [16]byte
	Wrapped   []byte
	CreatedAt time.Time
	DemotedAt time.Time
}

// KeyInfo is a data key's metadata as this Ring sees it. It never carries key material.
type KeyInfo struct {
	ID        [16]byte
	Scope     string
	State     State
	KEKID     [16]byte
	CreatedAt time.Time
	DemotedAt time.Time
	// Locked is true for a live key this Ring cannot unwrap, usually because its KEK is not in the Ring's set.
	Locked bool
}

// StaleReason says why a decrypted value should be rewritten.
type StaleReason string

const (
	// StaleLegacy marks a value read from the legacy shape.
	StaleLegacy StaleReason = "legacy"
	// StaleNotPrimary marks a value under a key that is not its scope's Primary.
	StaleNotPrimary StaleReason = "not_primary"
)

// Status accompanies a successful decrypt.
type Status struct {
	Stale  bool
	Reason StaleReason
}

var (
	// ErrConflict reports that the store changed underneath a conditional write.
	ErrConflict = errors.New("keyring: concurrent change")
	// ErrNotPrimary is what a product's store returns when a write names a key that is not its scope's Primary.
	ErrNotPrimary = errors.New("keyring: data key is not its scope's primary")
	// ErrScopeMismatch reports a value sealed under another scope's key.
	ErrScopeMismatch = errors.New("keyring: data key belongs to another scope")
	// ErrKEKUnavailable reports a key whose KEK this Ring does not hold, or that it could not unwrap.
	ErrKEKUnavailable = errors.New("keyring: key-encryption key not available")
	// ErrTargetKEKUnavailable reports that this Ring does not hold the target KEK, so it may not create data keys.
	ErrTargetKEKUnavailable = errors.New("keyring: target key-encryption key not available")
	// ErrDestroyed reports a value under a destroyed key.
	ErrDestroyed = errors.New("keyring: data key destroyed")
	// ErrUnknownKey reports a key id the store does not have.
	ErrUnknownKey = errors.New("keyring: unknown data key")
	// ErrIsPrimary refuses destroying a scope's Primary.
	ErrIsPrimary = errors.New("keyring: data key is its scope's primary")
	// ErrInUse refuses destroying a key that stored values still use.
	ErrInUse = errors.New("keyring: data key is in use")
	// ErrTooRecent refuses destroying a key demoted less than Options.DestroyAfter ago.
	ErrTooRecent = errors.New("keyring: data key demoted too recently")
	// ErrStoreMismatch reports that the store's read of a data key just created does not match what was written: another scope, a KEK this Ring does not hold, or wrapped bytes that do not unwrap to the new key.
	ErrStoreMismatch = errors.New("keyring: store returned a different data key than written")
)

// Store persists data keys and the target KEK. Every method must be safe for concurrent use; the conditional writes must be atomic.
type Store interface {
	// List returns every data key, destroyed ones included, as one consistent read.
	List(ctx context.Context) ([]DataKey, error)
	// Get returns one data key, or ErrUnknownKey.
	Get(ctx context.Context, id [16]byte) (DataKey, error)
	// Primary returns the scope's Primary, or ErrUnknownKey when the scope has none.
	Primary(ctx context.Context, scope string) (DataKey, error)
	// TargetKEK returns the KEK new data keys are wrapped under and rewrap moves towards; zero means unset.
	TargetKEK(ctx context.Context) ([16]byte, error)
	// SetTargetKEK sets the target only when it currently equals expected (zero = unset); otherwise ErrConflict.
	SetTargetKEK(ctx context.Context, id, expected [16]byte) error
	// InsertPrimary adds k (State Primary) and demotes the scope's previous Primary to DecryptOnly, setting its DemotedAt, in one transaction. It returns ErrConflict when the scope's current Primary is not expectedPrimary (nil = the scope has none).
	InsertPrimary(ctx context.Context, k DataKey, expectedPrimary *[16]byte) error
	// UpdateWrapped replaces a key's wrapped bytes and KEK id only while it is still under fromKEK and not destroyed; otherwise ErrConflict.
	UpdateWrapped(ctx context.Context, id, fromKEK, toKEK [16]byte, wrapped []byte) error
	// Destroy checks that no stored value uses the key and destroys it in one transaction: State Destroyed, Wrapped nil. It returns ErrIsPrimary for a Primary, ErrInUse when values use it, ErrUnknownKey for an unknown id, and nil when already destroyed.
	Destroy(ctx context.Context, id [16]byte) error
}

// Options tunes a Ring. Zero values take the defaults.
type Options struct {
	// DestroyAfter is how long a key stays DecryptOnly before Destroy accepts it. Default 24h.
	DestroyAfter time.Duration
	// RefreshInterval is how often Run reloads every key. Default 5m.
	RefreshInterval time.Duration
	// UnknownKeyTTL is how long an id the store did not have is remembered before Decrypt asks the store again. Default 5s.
	UnknownKeyTTL time.Duration
	// Now is the clock. Default time.Now.
	Now func() time.Time
	// ReloadError receives errors from the periodic reload in Run. Default: ignored.
	ReloadError func(error)
}

func (o Options) withDefaults() (Options, error) {
	if o.DestroyAfter < 0 || o.RefreshInterval < 0 || o.UnknownKeyTTL < 0 {
		return o, errors.New("keyring: negative duration in options")
	}
	if o.DestroyAfter == 0 {
		o.DestroyAfter = 24 * time.Hour
	}
	if o.RefreshInterval == 0 {
		o.RefreshInterval = 5 * time.Minute
	}
	if o.UnknownKeyTTL == 0 {
		o.UnknownKeyTTL = 5 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o, nil
}

// RewrapError lists the live keys Rewrap left under another KEK, normally because this Ring cannot unwrap them; rerun Rewrap where both their KEK and the target are held.
type RewrapError struct {
	Skipped [][16]byte
}

func (e *RewrapError) Error() string {
	ids := make([]string, len(e.Skipped))
	for i, id := range e.Skipped {
		ids[i] = FormatID(id)
	}
	return fmt.Sprintf("keyring: rewrap left %d data keys under another KEK: %v", len(ids), ids)
}

func (e *RewrapError) Unwrap() error { return ErrKEKUnavailable }
