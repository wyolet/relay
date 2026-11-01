package control

import (
	"context"
	"fmt"
	"slices"

	"github.com/wyolet/relay/app/meta"
)

// scanAccountRefs lists the host keys that name the account: removing it
// would leave each one pointing at an organization that no longer exists.
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

// scanUpstreamAtHost lists the accounts and pools held at a host; neither
// means anything without it.
func scanUpstreamAtHost(ctx context.Context, d Deps, hostID string) ([]referenceItem, error) {
	out := []referenceItem{}
	if d.Stores.Account != nil {
		accounts, err := d.Stores.Account.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("list accounts: %w", err)
		}
		for _, a := range accounts {
			if a.Spec.HostID == hostID {
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
		for _, a := range accounts {
			if ownedBy(a.Meta.Owner, kind, id) {
				out = append(out, referenceItem{Kind: "account", ID: a.Meta.ID, Name: a.Meta.Name, Via: "metadata.owner.id", owner: a.Meta.Owner})
			}
		}
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
