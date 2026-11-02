// Package share revokes a shared provider account. Revoking always succeeds: the policy bindings holding the share let go of it (a binding left with no account goes), relay keys whose lane it was move to their principal's own account and keep working, and pools drawing on it lose it. Each change is reported, so whoever revokes sees the impact without needing to see or edit the rows.
//
// Out of scope: who may revoke (the callers check), and making shares (app/refcheck and the control API).
package share

import (
	"context"
	"fmt"
	"slices"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
)

// Change is one row a revoke changes besides the share itself.
type Change struct {
	// Kind is the row's API singular: policy-binding, key or pool.
	Kind  string
	ID    string
	Name  string
	Owner meta.Owner
	// Field is the field that named the share.
	Field string
	// Deleted is set on a policy binding the revoke left with no account, which goes.
	Deleted bool
}

// Rows are what a revoke reads.
type Rows struct {
	Bindings []*policybinding.PolicyBinding
	Keys     []*key.Key
	Pools    []*pool.Pool
}

// Impact is what revoking share id changes in rows, in the order Revoke makes the changes.
func Impact(r Rows, id string) []Change {
	var out []Change
	for _, b := range r.Bindings {
		if slices.Contains(b.Spec.AccountIDs, id) {
			out = append(out, Change{Kind: "policy-binding", ID: b.Meta.ID, Name: b.Meta.Name, Owner: b.Meta.Owner, Field: "spec.accountIds", Deleted: len(b.Spec.AccountIDs) == 1})
		}
	}
	for _, k := range r.Keys {
		if k.Spec.AccountID == id {
			out = append(out, Change{Kind: "key", ID: k.Meta.ID, Name: k.Meta.Name, Owner: k.Meta.Owner, Field: "spec.accountId"})
		}
	}
	for _, p := range r.Pools {
		if slices.Contains(p.Spec.AccountIDs, id) {
			out = append(out, Change{Kind: "pool", ID: p.Meta.ID, Name: p.Meta.Name, Owner: p.Meta.Owner, Field: "spec.accountIds"})
		}
	}
	return out
}

// Stores are what a revoke writes through; the caller runs Revoke inside one transaction.
type Stores struct {
	Account       *account.Store
	PolicyBinding *policybinding.Store
	Key           *key.Store
	Pool          *pool.Store
	// OwnAccount returns the id of a principal's own account (a user's personal account, a service account's account), making it on first use.
	OwnAccount func(ctx context.Context, p key.Principal) (string, error)
}

// Revoke deletes share id and makes every change Impact reports. A row that is not a share is refused.
func Revoke(ctx context.Context, s Stores, id string) ([]Change, error) {
	a, err := s.Account.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("share.Revoke: %w", err)
	}
	if a == nil {
		return nil, nil
	}
	if !a.IsShared() {
		return nil, fmt.Errorf("share.Revoke: account %q is not a share", a.Meta.Name)
	}
	var r Rows
	if r.Bindings, err = s.PolicyBinding.List(ctx); err != nil {
		return nil, fmt.Errorf("share.Revoke: list policy bindings: %w", err)
	}
	if r.Keys, err = s.Key.List(ctx); err != nil {
		return nil, fmt.Errorf("share.Revoke: list keys: %w", err)
	}
	if s.Pool != nil {
		if r.Pools, err = s.Pool.List(ctx); err != nil {
			return nil, fmt.Errorf("share.Revoke: list pools: %w", err)
		}
	}
	changes := Impact(r, id)
	for _, c := range changes {
		if err := s.apply(ctx, c, id); err != nil {
			return nil, fmt.Errorf("share.Revoke: %s %q: %w", c.Kind, c.Name, err)
		}
	}
	// Pools lose the share through the foreign key.
	if err := s.Account.Delete(ctx, id); err != nil {
		return nil, fmt.Errorf("share.Revoke: %w", err)
	}
	return changes, nil
}

func (s Stores) apply(ctx context.Context, c Change, id string) error {
	switch c.Kind {
	case "policy-binding":
		if c.Deleted {
			return s.PolicyBinding.Delete(ctx, c.ID)
		}
		b, err := s.PolicyBinding.Get(ctx, c.ID)
		if err != nil || b == nil {
			return err
		}
		b.Spec.AccountIDs = slices.DeleteFunc(b.Spec.AccountIDs, func(x string) bool { return x == id })
		b.Meta.Dirty = true
		return s.PolicyBinding.Upsert(ctx, b)
	case "key":
		k, err := s.Key.Get(ctx, c.ID)
		if err != nil || k == nil {
			return err
		}
		k.Spec.AccountID = ""
		if s.OwnAccount != nil {
			if k.Spec.AccountID, err = s.OwnAccount(ctx, k.Spec.Principal); err != nil {
				return err
			}
		}
		k.Meta.Dirty = true
		return s.Key.Upsert(ctx, k)
	}
	return nil
}
