package control

import (
	"context"
	"fmt"
	"slices"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/meta"
)

// scanAccountRefs lists the host keys that name the account, which would point at an organization that no longer exists, the accounts shared from it, which detach deletes, the relay keys whose lane it is, which would go with it, and the policy bindings holding it.
func scanAccountRefs(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
	out := []referenceItem{}
	keys, err := d.Stores.HostKey.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list host-keys: %w", err)
	}
	for _, k := range keys {
		if k.Spec.AccountID == id {
			out = append(out, referenceItem{Kind: "host-key", ID: k.Meta.ID, Name: k.Meta.Name, Via: "spec.accountId", owner: k.Meta.Owner})
		}
	}
	shares, err := sharesFrom(ctx, d, id)
	if err != nil {
		return nil, err
	}
	out = append(out, shares...)
	lanes, err := keysInAccounts(ctx, d, func(accountID string) bool { return accountID == id })
	if err != nil {
		return nil, err
	}
	held, err := bindingsHolding(ctx, d, id)
	if err != nil {
		return nil, err
	}
	return append(append(out, lanes...), held...), nil
}

// sharesFrom lists the accounts shared from the account id. Each lives in its receiver's scope, and is named to a caller who may share the real account.
func sharesFrom(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
	accounts, err := d.Stores.Account.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	var out []referenceItem
	sharer := false
	for _, a := range accounts {
		if a.Meta.ID == id {
			sharer = refs(d).MayShare(ctx, a) == nil
		}
	}
	for _, a := range accounts {
		if a.Spec.SourceAccountID == id {
			out = append(out, referenceItem{Kind: "account", ID: a.Meta.ID, Name: a.Meta.Name, Via: "spec.sourceAccountId", owner: a.Meta.Owner, detachable: true, shownToSharer: sharer})
		}
	}
	return out, nil
}

// poolsDrawingOn lists the pools an account's delete narrows: the account drops out of each, and the pool draws on what it still lists.
func poolsDrawingOn(ctx context.Context, d Deps, accountID string) ([]referenceItem, error) {
	if d.Stores.Pool == nil {
		return nil, nil
	}
	pools, err := d.Stores.Pool.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pools: %w", err)
	}
	var out []referenceItem
	for _, p := range pools {
		if slices.Contains(p.Spec.AccountIDs, accountID) {
			out = append(out, referenceItem{Kind: "pool", ID: p.Meta.ID, Name: p.Meta.Name, Via: "spec.accountIds", owner: p.Meta.Owner})
		}
	}
	return out, nil
}

// keysInAccounts lists the relay keys whose default lane in matches: a lane's delete would take them with it.
func keysInAccounts(ctx context.Context, d Deps, in func(accountID string) bool) ([]referenceItem, error) {
	out := []referenceItem{}
	if d.Stores.Key == nil {
		return out, nil
	}
	keys, err := d.Stores.Key.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	for _, k := range keys {
		if k.Spec.AccountID != "" && in(k.Spec.AccountID) {
			out = append(out, referenceItem{Kind: "key", ID: k.Meta.ID, Name: k.Meta.Name, Via: "spec.accountId", owner: k.Meta.Owner})
		}
	}
	return out, nil
}

// scanPoolRefs and scanBudgetRefs find nothing that blocks a delete: the
// policies linking the row lose the link (droppedLinkScans).
func scanPoolRefs(context.Context, Deps, string) ([]referenceItem, error) {
	return []referenceItem{}, nil
}

func scanBudgetRefs(context.Context, Deps, string) ([]referenceItem, error) {
	return []referenceItem{}, nil
}

// accountsInUse reports whether a provider account blocks the delete of its host or owner. A default account no key names does not: autofill made it and the next key recreates it, so it goes with the host (foreign key) or the owner (000036's trigger). A caller account never does itself: it goes with its team or project, and the keys in it are listed instead.
func accountsInUse(ctx context.Context, d Deps) (func(*account.Account) bool, error) {
	keys, err := d.Stores.HostKey.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list host-keys: %w", err)
	}
	held := make(map[string]bool, len(keys))
	for _, k := range keys {
		held[k.Spec.AccountID] = true
	}
	return func(a *account.Account) bool { return a.IsProvider() && (!a.Spec.Default || held[a.Meta.ID]) }, nil
}

// scanUpstreamAtHost lists the accounts and pools held at a host; neither
// means anything without it.
func scanUpstreamAtHost(ctx context.Context, d Deps, hostID string) ([]referenceItem, error) {
	out := []referenceItem{}
	if d.Stores.Account != nil {
		accounts, err := d.Stores.Account.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("list accounts: %w", err)
		}
		inUse, err := accountsInUse(ctx, d)
		if err != nil {
			return nil, err
		}
		for _, a := range accounts {
			if a.Spec.HostID == hostID && inUse(a) {
				out = append(out, referenceItem{Kind: "account", ID: a.Meta.ID, Name: a.Meta.Name, Via: "spec.hostId", owner: a.Meta.Owner})
			}
		}
	}
	if d.Stores.Pool != nil {
		pools, err := d.Stores.Pool.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("list pools: %w", err)
		}
		for _, p := range pools {
			if p.Spec.HostID == hostID {
				out = append(out, referenceItem{Kind: "pool", ID: p.Meta.ID, Name: p.Meta.Name, Via: "spec.hostId", owner: p.Meta.Owner})
			}
		}
	}
	return out, nil
}

// scanUpstreamOwnedBy lists the accounts, pools and budgets a team or
// project owns, which its delete would orphan.
func scanUpstreamOwnedBy(ctx context.Context, d Deps, kind meta.OwnerKind, id string) ([]referenceItem, error) {
	out := []referenceItem{}
	if d.Stores.Account != nil {
		accounts, err := d.Stores.Account.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("list accounts: %w", err)
		}
		inUse, err := accountsInUse(ctx, d)
		if err != nil {
			return nil, err
		}
		callers := map[string]bool{}
		for _, a := range accounts {
			switch {
			case !ownedBy(a.Meta.Owner, kind, id):
			case !a.IsProvider():
				callers[a.Meta.ID] = true
			case inUse(a):
				out = append(out, referenceItem{Kind: "account", ID: a.Meta.ID, Name: a.Meta.Name, Via: "metadata.owner.id", owner: a.Meta.Owner})
			}
		}
		lanes, err := keysInAccounts(ctx, d, func(accountID string) bool { return callers[accountID] })
		if err != nil {
			return nil, err
		}
		out = append(out, lanes...)
	}
	if d.Stores.Pool != nil {
		pools, err := d.Stores.Pool.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("list pools: %w", err)
		}
		for _, p := range pools {
			if ownedBy(p.Meta.Owner, kind, id) {
				out = append(out, referenceItem{Kind: "pool", ID: p.Meta.ID, Name: p.Meta.Name, Via: "metadata.owner.id", owner: p.Meta.Owner})
			}
		}
	}
	if d.Stores.Budget != nil {
		budgets, err := d.Stores.Budget.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("list budgets: %w", err)
		}
		for _, b := range budgets {
			if ownedBy(b.Meta.Owner, kind, id) {
				out = append(out, referenceItem{Kind: "budget", ID: b.Meta.ID, Name: b.Meta.Name, Via: "metadata.owner.id", owner: b.Meta.Owner})
			}
		}
	}
	return out, nil
}

// poolsHolding lists the pools a host key's delete narrows: the key drops
// out of each and the pool draws on the keys it still has.
func poolsHolding(ctx context.Context, d Deps, keyID string) ([]referenceItem, error) {
	if d.Stores.Pool == nil {
		return nil, nil
	}
	pools, err := d.Stores.Pool.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pools: %w", err)
	}
	var out []referenceItem
	for _, p := range pools {
		if slices.Contains(p.Spec.CredentialIDs, keyID) {
			out = append(out, referenceItem{Kind: "pool", ID: p.Meta.ID, Name: p.Meta.Name, Via: "spec.credentialIds", owner: p.Meta.Owner})
		}
	}
	return out, nil
}
