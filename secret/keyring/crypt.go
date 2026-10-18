package keyring

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"

	"github.com/wyolet/relay/secret/aead"
)

// Encrypt seals plaintext under the scope's Primary, creating the scope's first data key when it has none. It returns the id of the key used, which the product stores so its write can be checked against key state.
//
// A new data key is wrapped under the store's target KEK; when no target is set yet, the first KEK passed to kek.NewSet becomes it. Encrypt returns ErrTargetKEKUnavailable rather than create a key under any other KEK, and ErrKEKUnavailable when the scope's Primary is under a KEK this Ring does not hold.
func (r *Ring) Encrypt(ctx context.Context, scope string, plaintext, aad []byte) (sealed []byte, keyID [16]byte, err error) {
	e, err := r.primaryFor(ctx, scope)
	if err != nil {
		return nil, keyID, err
	}
	sealed, err = aead.Seal(e.dek, e.key.ID, plaintext, aad)
	if err != nil {
		return nil, keyID, err
	}
	return sealed, e.key.ID, nil
}

// EncryptAndWrite encrypts and hands the result to write. When write reports ErrNotPrimary (the key was demoted and this Ring had not heard yet), it reloads the scope and encrypts and writes once more.
func (r *Ring) EncryptAndWrite(ctx context.Context, scope string, plaintext, aad []byte, write func(sealed []byte, keyID [16]byte) error) error {
	for attempt := 0; ; attempt++ {
		sealed, id, err := r.Encrypt(ctx, scope, plaintext, aad)
		if err != nil {
			return err
		}
		err = write(sealed, id)
		if attempt > 0 || !errors.Is(err, ErrNotPrimary) {
			return err
		}
		if err := r.reloadScope(ctx, scope, id); err != nil {
			return err
		}
	}
}

// Decrypt opens a value sealed by Encrypt under scope with the same aad. Status.Stale is set when the key is no longer its scope's Primary.
func (r *Ring) Decrypt(ctx context.Context, scope string, sealed, aad []byte) ([]byte, Status, error) {
	id, err := aead.KeyID(sealed)
	if err != nil {
		return nil, Status{}, err
	}
	v, e, err := r.lookup(ctx, id)
	if err != nil {
		return nil, Status{}, err
	}
	switch {
	case e.key.Scope != scope:
		return nil, Status{}, fmt.Errorf("%w: key %s", ErrScopeMismatch, FormatID(id))
	case e.key.State == Destroyed:
		return nil, Status{}, fmt.Errorf("%w: key %s", ErrDestroyed, FormatID(id))
	case e.dek == nil:
		return nil, Status{}, fmt.Errorf("%w: key %s", ErrKEKUnavailable, FormatID(id))
	}
	plain, err := aead.Open(e.dek, sealed, aad)
	if err != nil {
		return nil, Status{}, err
	}
	if p, ok := v.primary[scope]; !ok || p != id {
		return plain, Status{Stale: true, Reason: StaleNotPrimary}, nil
	}
	return plain, Status{}, nil
}

// DecryptLegacy opens the legacy shape with every KEK that supports it. The result is always stale.
func (r *Ring) DecryptLegacy(ciphertext, nonce []byte) ([]byte, Status, error) {
	if len(r.legacy) == 0 {
		return nil, Status{}, fmt.Errorf("%w: no KEK opens the legacy shape", ErrKEKUnavailable)
	}
	var lastErr error
	for _, lo := range r.legacy {
		plain, err := lo.OpenLegacy(ciphertext, nonce)
		if err == nil {
			return plain, Status{Stale: true, Reason: StaleLegacy}, nil
		}
		lastErr = err
	}
	return nil, Status{}, fmt.Errorf("keyring: no KEK opens the legacy value: %w", lastErr)
}

// primaryFor returns the scope's usable Primary, reading the store before creating one so a key made elsewhere is found first. A locked Primary is read again too: a rewrap this Ring missed may have moved it under a KEK it holds.
func (r *Ring) primaryFor(ctx context.Context, scope string) (*entry, error) {
	if e, ok := r.viewPrimary(scope); ok && e.dek != nil {
		return e, nil
	}
	if err := r.reloadScope(ctx, scope); err != nil {
		return nil, err
	}
	if e, ok := r.viewPrimary(scope); ok {
		return usable(e)
	}
	e, err := r.createPrimary(ctx, scope, nil)
	if !errors.Is(err, ErrConflict) {
		return e, err
	}
	if err := r.reloadScope(ctx, scope); err != nil {
		return nil, err
	}
	if e, ok := r.viewPrimary(scope); ok {
		return usable(e)
	}
	return nil, err
}

func (r *Ring) viewPrimary(scope string) (*entry, bool) {
	v := r.view.Load()
	id, ok := v.primary[scope]
	if !ok {
		return nil, false
	}
	return v.keys[id], true
}

// usable refuses a locked Primary: creating a new key over it would strand the values under it for every process that holds its KEK.
func usable(e *entry) (*entry, error) {
	if e.dek == nil {
		return nil, fmt.Errorf("%w: primary key %s of scope %q", ErrKEKUnavailable, FormatID(e.key.ID), e.key.Scope)
	}
	return e, nil
}

// createPrimary makes a data key under the target KEK and inserts it as the scope's Primary, conditional on expected being the current one.
func (r *Ring) createPrimary(ctx context.Context, scope string, expected *[16]byte) (*entry, error) {
	target, err := r.ensureTarget(ctx)
	if err != nil {
		return nil, err
	}
	k, ok := r.keks.Get(target)
	if !ok {
		return nil, fmt.Errorf("%w: target %x", ErrTargetKEKUnavailable, target)
	}
	now := r.opts.Now()
	id, err := newKeyID(now)
	if err != nil {
		return nil, err
	}
	dek := make([]byte, aead.KeySize)
	if _, err := rand.Read(dek); err != nil {
		return nil, fmt.Errorf("keyring: generate data key: %w", err)
	}
	wrapped, err := k.Wrap(ctx, dek, wrapAAD(id, scope))
	if err != nil {
		return nil, fmt.Errorf("keyring: wrap data key: %w", err)
	}
	dk := DataKey{ID: id, Scope: scope, State: Primary, KEKID: target, Wrapped: wrapped, CreatedAt: now}
	if err := r.store.InsertPrimary(ctx, dk, expected); err != nil {
		return nil, err
	}
	ids := [][16]byte{id}
	if expected != nil {
		ids = append(ids, *expected)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.reloadIDsLocked(ctx, nil, ids...); err != nil {
		return nil, err
	}
	// The new key is used only as the store returns it, unwrapped from the stored bytes, so a store that saved or answered something else cannot leave this Ring writing values other processes cannot read. Any held KEK is accepted: a concurrent rewrap may already have moved the key.
	e := r.view.Load().keys[id]
	switch {
	case e == nil || e.key.Scope != scope || e.dek == nil || subtle.ConstantTimeCompare(e.dek, dek) != 1:
		return nil, fmt.Errorf("%w: data key %s of scope %q", ErrStoreMismatch, FormatID(id), scope)
	case e.key.State != Primary:
		// Demoted by another process between the insert and the read.
		return nil, fmt.Errorf("%w: data key %s demoted after its insert", ErrConflict, FormatID(id))
	}
	return e, nil
}

// ensureTarget reads the target KEK, setting it to the first KEK passed to kek.NewSet when the store has none yet (the first data key ever).
func (r *Ring) ensureTarget(ctx context.Context) ([16]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, err := r.store.TargetKEK(ctx)
	if err != nil {
		return t, err
	}
	if t == ([16]byte{}) {
		first := r.keks.IDs()[0]
		switch err := r.store.SetTargetKEK(ctx, first, [16]byte{}); {
		case err == nil:
			t = first
		case errors.Is(err, ErrConflict):
			if t, err = r.store.TargetKEK(ctx); err != nil {
				return t, err
			}
		default:
			return t, err
		}
	}
	r.update(func(v *view) { v.target = t })
	return t, nil
}
