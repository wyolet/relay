package keyring

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
)

// Rotate makes a new Primary for scope and demotes the current one to DecryptOnly. It returns ErrConflict when another process changed the scope's Primary at the same time.
func (r *Ring) Rotate(ctx context.Context, scope string) (DataKey, error) {
	if err := r.reloadScope(ctx, scope); err != nil {
		return DataKey{}, err
	}
	var expected *[16]byte
	if p, ok := r.view.Load().primary[scope]; ok {
		expected = &p
	}
	e, err := r.createPrimary(ctx, scope, expected)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			if rerr := r.reloadScope(ctx, scope); rerr != nil {
				return DataKey{}, errors.Join(err, rerr)
			}
		}
		return DataKey{}, err
	}
	k := e.key
	k.Wrapped = bytes.Clone(k.Wrapped)
	return k, nil
}

// Rewrap makes to the target KEK and rewraps every live data key that is under another KEK. The DEKs themselves do not change. It returns ErrTargetKEKUnavailable when to is not in this Ring's set. Keys this Ring cannot unwrap are skipped and reported in a *RewrapError; the count covers the keys rewrapped here.
func (r *Ring) Rewrap(ctx context.Context, to [16]byte) (int, error) {
	toKEK, ok := r.keks.Get(to)
	if !ok {
		return 0, fmt.Errorf("%w: rewrap target %x is not in this ring's set", ErrTargetKEKUnavailable, to)
	}
	if err := r.moveTarget(ctx, to); err != nil {
		return 0, err
	}
	done := 0
	// Later passes pick up keys created under the previous target by processes that had not seen the move yet.
	for pass := 0; ; pass++ {
		if err := r.ReloadAll(ctx); err != nil {
			return done, err
		}
		var todo []*entry
		for _, e := range r.view.Load().keys {
			if e.key.State != Destroyed && e.key.KEKID != to && e.dek != nil {
				todo = append(todo, e)
			}
		}
		if len(todo) == 0 || pass == 3 {
			break
		}
		for _, e := range todo {
			wrapped, err := toKEK.Wrap(ctx, e.dek, wrapAAD(e.key.ID, e.key.Scope))
			if err != nil {
				return done, fmt.Errorf("keyring: wrap data key %s: %w", FormatID(e.key.ID), err)
			}
			err = r.store.UpdateWrapped(ctx, e.key.ID, e.key.KEKID, to, wrapped)
			if errors.Is(err, ErrConflict) {
				continue
			}
			if err != nil {
				return done, err
			}
			done++
		}
	}
	var skipped [][16]byte
	for id, e := range r.view.Load().keys {
		if e.key.State != Destroyed && e.key.KEKID != to {
			skipped = append(skipped, id)
		}
	}
	if len(skipped) > 0 {
		slices.SortFunc(skipped, func(a, b [16]byte) int { return bytes.Compare(a[:], b[:]) })
		return done, &RewrapError{Skipped: skipped}
	}
	return done, nil
}

func (r *Ring) moveTarget(ctx context.Context, to [16]byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, err := r.store.TargetKEK(ctx)
	if err != nil {
		return err
	}
	if cur != to {
		if err := r.store.SetTargetKEK(ctx, to, cur); err != nil {
			return err
		}
	}
	r.update(func(v *view) { v.target = to })
	return nil
}

// Destroy destroys a DecryptOnly key that no stored value uses and that was demoted at least Options.DestroyAfter ago. Destroying a destroyed key is a no-op.
func (r *Ring) Destroy(ctx context.Context, id [16]byte) error {
	return r.destroy(ctx, id, false)
}

// ForceDestroy is Destroy without the DestroyAfter wait, for a compromised key. It still refuses a Primary and a key in use.
func (r *Ring) ForceDestroy(ctx context.Context, id [16]byte) error {
	return r.destroy(ctx, id, true)
}

func (r *Ring) destroy(ctx context.Context, id [16]byte, force bool) error {
	k, err := r.store.Get(ctx, id)
	if err != nil {
		return err
	}
	switch k.State {
	case Destroyed:
		return r.Reload(ctx, id)
	case Primary:
		return fmt.Errorf("%w: key %s", ErrIsPrimary, FormatID(id))
	}
	// A key never becomes Primary again and DemotedAt is set once, so this check cannot go stale before the store's Destroy.
	minAge := r.opts.DestroyAfter
	if force {
		minAge = 0
	} else if k.DemotedAt.IsZero() || r.opts.Now().Sub(k.DemotedAt) < minAge {
		return fmt.Errorf("%w: key %s demoted at %s", ErrTooRecent, FormatID(id), k.DemotedAt)
	}
	if err := r.store.Destroy(ctx, id, minAge); err != nil {
		return err
	}
	return r.Reload(ctx, id)
}

// Keys returns every key this Ring knows, ordered by scope and creation time.
func (r *Ring) Keys() []KeyInfo {
	v := r.view.Load()
	out := make([]KeyInfo, 0, len(v.keys))
	for _, e := range v.keys {
		out = append(out, KeyInfo{
			ID:        e.key.ID,
			Scope:     e.key.Scope,
			State:     e.key.State,
			KEKID:     e.key.KEKID,
			CreatedAt: e.key.CreatedAt,
			DemotedAt: e.key.DemotedAt,
			Locked:    e.key.State != Destroyed && e.dek == nil,
		})
	}
	slices.SortFunc(out, func(a, b KeyInfo) int {
		return cmp.Or(cmp.Compare(a.Scope, b.Scope), a.CreatedAt.Compare(b.CreatedAt), bytes.Compare(a.ID[:], b.ID[:]))
	})
	return out
}

// Target returns the target KEK id as of this Ring's last read; zero means unset.
func (r *Ring) Target() [16]byte {
	return r.view.Load().target
}
