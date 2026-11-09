package control

import (
	"context"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pool"
)

// enrichHostStatus returns an enrichFn that overlays observed runtime health
// (host.Status) onto a freshly-loaded Host from the host-health store. The
// field is derived (json:"status", yaml:"-") and never persisted; nil when no
// observation exists yet (no traffic / TTL'd out) so the UI shows "unknown".
func enrichHostStatus(d Deps) enrichFn[host.Host] {
	return func(ctx context.Context, h *host.Host) {
		if h == nil || d.HostHealth == nil {
			return
		}
		if st, found := d.HostHealth.Read(ctx, h.Meta.ID); found {
			s := st
			h.Status = &s
		}
	}
}

// enrichHostStatusAll is the list-path variant: one kv Range for every host's
// health record instead of a Get per row.
func enrichHostStatusAll(d Deps) enrichListFn[host.Host] {
	return func(ctx context.Context, hosts []*host.Host) {
		if len(hosts) == 0 || d.HostHealth == nil {
			return
		}
		statuses := d.HostHealth.ReadAll(ctx)
		if len(statuses) == 0 {
			return
		}
		for _, h := range hosts {
			if st, found := statuses[h.Meta.ID]; found {
				s := st
				h.Status = &s
			}
		}
	}
}

// enrichHostKeyPolicies returns an enrichFn that fills HostKey.Policies
// with the Policies linking this key, directly or through a pool, read
// from the stores. Reverse-ref summary for the admin UI; never persisted
// (the field is yaml:"-" and skipped by the store).
func enrichHostKeyPolicies(d Deps) enrichFn[hostkey.HostKey] {
	all := enrichHostKeyPoliciesAll(d)
	return func(ctx context.Context, k *hostkey.HostKey) {
		if k != nil {
			all(ctx, []*hostkey.HostKey{k})
		}
	}
}

// enrichHostKeyPoliciesAll is the list-path variant: one Policy.List, one
// Pool.List and one Account.List for the whole page instead of one per key
// row. A pool reaches a key it lists, and every key of an account it draws on
// (a shared account's: its real account's).
func enrichHostKeyPoliciesAll(d Deps) enrichListFn[hostkey.HostKey] {
	return func(ctx context.Context, keys []*hostkey.HostKey) {
		if len(keys) == 0 || d.Stores == nil || d.Stores.Policy == nil || d.Stores.Pool == nil {
			return
		}
		pols, err := d.Stores.Policy.List(ctx)
		if err != nil {
			return
		}
		pools, err := d.Stores.Pool.List(ctx)
		if err != nil {
			return
		}
		poolByID := make(map[string]*pool.Pool, len(pools))
		for _, pl := range pools {
			poolByID[pl.Meta.ID] = pl
		}
		accountByID := map[string]*account.Account{}
		if d.Stores.Account != nil {
			accounts, err := d.Stores.Account.List(ctx)
			if err != nil {
				return
			}
			for _, a := range accounts {
				accountByID[a.Meta.ID] = a
			}
		}
		// A share must not name its receiver's policies to the sharer.
		visible := func(*policy.Policy) bool { return true }
		if s, ok := d.Authz.(authz.Scoper); ok {
			visible = func(p *policy.Policy) bool { return s.Visible(ctx, "policy", p.Meta.ID, p.Meta.Owner) }
		}
		for _, k := range keys {
			k.Policies = nil
		}
		for _, p := range pols {
			listed, ownAccounts, sharedFrom := map[string]bool{}, map[string]bool{}, map[string]bool{}
			for _, pid := range p.Spec.PoolIDs {
				pl := poolByID[pid]
				if pl == nil {
					continue
				}
				for _, id := range pl.Spec.CredentialIDs {
					listed[id] = true
				}
				for _, aid := range pl.Spec.AccountIDs {
					switch a := accountByID[aid]; {
					case a == nil:
					case a.IsShared():
						sharedFrom[a.Spec.SourceAccountID] = true
					default:
						ownAccounts[aid] = true
					}
				}
			}
			viaShare := len(sharedFrom) > 0 && visible(p)
			for _, k := range keys {
				acct := k.Spec.AccountID
				if listed[k.Meta.ID] || (acct != "" && (ownAccounts[acct] || (viaShare && sharedFrom[acct]))) {
					k.Policies = append(k.Policies, hostkey.PolicyRef{ID: p.Meta.ID, Name: p.Meta.Name})
				}
			}
		}
	}
}
