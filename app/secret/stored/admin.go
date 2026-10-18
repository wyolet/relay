package stored

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wyolet/relay/secret/keyring"
)

// DestroyAfter is how long a demoted data key waits before Destroy accepts it, so a process that has not yet seen the rotation cannot lose a value it is about to write.
const DestroyAfter = 24 * time.Hour

var (
	// ErrLegacyRows refuses a rewrap while stored secrets remain in the legacy shape: they are encrypted under the master key itself, so after a master key change only the old key would open them.
	ErrLegacyRows = errors.New("stored secrets remain in the legacy master-key shape")
	// ErrNoScopeKey refuses rotating a scope that has no data key, usually a typo: a scope gets its first key when a secret is first stored in it.
	ErrNoScopeKey = errors.New("scope has no data key")
	// ErrBadKeyID reports a data key or KEK id that does not parse.
	ErrBadKeyID = errors.New("malformed key id")
)

// RewrapResult reports a rewrap. Skipped lists live data keys left under another KEK because this process cannot unwrap them; run the rewrap again where both that KEK and the target are held.
type RewrapResult struct {
	Target    string   `json:"target"`
	Rewrapped int      `json:"rewrapped"`
	Skipped   []string `json:"skipped"`
}

// Rotate gives scope a new Primary data key and demotes the current one. Values under the old key stay readable; the rewriter moves them to the new one.
func (s *Backend) Rotate(ctx context.Context, scope string) (DataKeyStatus, error) {
	ring, err := s.keys.load(ctx)
	if err != nil {
		return DataKeyStatus{}, err
	}
	if _, err := s.keys.store.Primary(ctx, scope); errors.Is(err, keyring.ErrUnknownKey) {
		return DataKeyStatus{}, fmt.Errorf("%w: %q", ErrNoScopeKey, scope)
	} else if err != nil {
		return DataKeyStatus{}, err
	}
	return rotate(ctx, ring, scope)
}

// RotateAll rotates every scope that has a Primary, in scope order. A scope that fails does not stop the others: it returns every key it made together with the failures.
func (s *Backend) RotateAll(ctx context.Context) ([]DataKeyStatus, error) {
	ring, err := s.keys.load(ctx)
	if err != nil {
		return nil, err
	}
	if err := ring.ReloadAll(ctx); err != nil {
		return nil, err
	}
	var scopes []string
	for _, k := range ring.Keys() {
		if k.State == keyring.Primary {
			scopes = append(scopes, k.Scope)
		}
	}
	out := make([]DataKeyStatus, 0, len(scopes))
	var errs []error
	for _, scope := range scopes {
		k, err := rotate(ctx, ring, scope)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, k)
	}
	if len(errs) > 0 {
		return out, fmt.Errorf("rotated %d of %d scopes: %w", len(out), len(scopes), errors.Join(errs...))
	}
	return out, nil
}

func rotate(ctx context.Context, ring *keyring.Ring, scope string) (DataKeyStatus, error) {
	k, err := ring.Rotate(ctx, scope)
	if err != nil {
		return DataKeyStatus{}, fmt.Errorf("rotate scope %q: %w", scope, err)
	}
	return dataKeyStatus(keyring.KeyInfo{ID: k.ID, Scope: k.Scope, State: k.State, KEKID: k.KEKID, CreatedAt: k.CreatedAt}, 0), nil
}

// Rewrap makes the KEK named by to the target and rewraps every data key under it; the data keys themselves, and so every stored value, stay as they are. It refuses a KEK this process does not hold (keyring.ErrTargetKEKUnavailable), and refuses while legacy rows remain (ErrLegacyRows).
func (s *Backend) Rewrap(ctx context.Context, to string) (RewrapResult, error) {
	id, err := ParseKEKID(to)
	if err != nil {
		return RewrapResult{}, err
	}
	if s.keys.set == nil {
		return RewrapResult{}, ErrNoMasterKey
	}
	if _, ok := s.keys.set.Get(id); !ok {
		return RewrapResult{}, fmt.Errorf("%w: %s is not one of this process's master keys (RELAY_MASTER_KEY, RELAY_MASTER_KEY_EXTRA)",
			keyring.ErrTargetKEKUnavailable, FormatKEKID(id))
	}
	n, err := s.q.CountLegacySecretValues(ctx)
	if err != nil {
		return RewrapResult{}, fmt.Errorf("secret: count legacy rows: %w", err)
	}
	if n > 0 {
		return RewrapResult{}, fmt.Errorf("%w: %d rows; wait for the rewriter or run `relay secret migrate`, then rewrap", ErrLegacyRows, n)
	}
	ring, err := s.keys.load(ctx)
	if err != nil {
		return RewrapResult{}, err
	}
	done, err := ring.Rewrap(ctx, id)
	res := RewrapResult{Target: FormatKEKID(id), Rewrapped: done, Skipped: []string{}}
	if skipped, ok := errors.AsType[*keyring.RewrapError](err); ok {
		for _, k := range skipped.Skipped {
			res.Skipped = append(res.Skipped, keyring.FormatID(k))
		}
		return res, nil
	}
	return res, err
}

// Destroy destroys a demoted data key that no stored secret uses, keeping a tombstone so a value found later under it fails as destroyed. Without force the key must have been demoted at least DestroyAfter ago; force skips only that wait, for a compromised key.
func (s *Backend) Destroy(ctx context.Context, id string, force bool) (DataKeyStatus, error) {
	kid, err := ParseKeyID(id)
	if err != nil {
		return DataKeyStatus{}, err
	}
	ring, err := s.keys.load(ctx)
	if err != nil {
		return DataKeyStatus{}, err
	}
	if force {
		err = ring.ForceDestroy(ctx, kid)
	} else {
		err = ring.Destroy(ctx, kid)
	}
	if err != nil {
		return DataKeyStatus{}, destroyError(keyring.FormatID(kid), err)
	}
	for _, k := range ring.Keys() {
		if k.ID == kid {
			return dataKeyStatus(k, 0), nil
		}
	}
	return DataKeyStatus{}, fmt.Errorf("data key %s: %w", keyring.FormatID(kid), keyring.ErrUnknownKey)
}

func destroyError(id string, err error) error {
	hint := ""
	switch {
	case errors.Is(err, keyring.ErrIsPrimary):
		hint = "; rotate its scope first, then destroy the demoted key"
	case errors.Is(err, keyring.ErrInUse):
		hint = "; stored secrets still use it, wait until its row count reaches 0 (the rewriter moves them)"
	case errors.Is(err, keyring.ErrTooRecent):
		hint = fmt.Sprintf("; a data key can be destroyed %s after it was demoted, and force skips the wait for a compromised key", DestroyAfter)
	}
	return fmt.Errorf("data key %s: %w%s", id, err, hint)
}

// FormatKEKID renders a KEK id the way status reports it and rewrap accepts it: 32 hex digits.
func FormatKEKID(id [16]byte) string { return hex.EncodeToString(id[:]) }

// ParseKEKID parses a KEK id as FormatKEKID renders it.
func ParseKEKID(s string) ([16]byte, error) {
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != 16 {
		return [16]byte{}, fmt.Errorf("%w: KEK id %q is not 32 hex digits", ErrBadKeyID, s)
	}
	return [16]byte(b), nil
}

// ParseKeyID parses a data key id in the UUID form status reports.
func ParseKeyID(s string) ([16]byte, error) {
	s = strings.TrimSpace(s)
	b, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil || len(b) != 16 || len(s) != 36 || keyring.FormatID([16]byte(b)) != strings.ToLower(s) {
		return [16]byte{}, fmt.Errorf("%w: data key id %q is not a UUID", ErrBadKeyID, s)
	}
	return [16]byte(b), nil
}
