package control

import (
	"context"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
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

// enrichHostKeyPoliciesAll is the list-path variant: one Policy.List and
// one Pool.List for the whole page instead of one per key row.
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
		poolKeys := make(map[string][]string, len(pools))
		for _, pl := range pools {
			poolKeys[pl.Meta.ID] = pl.Spec.CredentialIDs
		}
		byKey := map[string][]hostkey.PolicyRef{}
		for _, p := range pols {
			seen := map[string]bool{}
			link := func(id string) {
				if !seen[id] {
					seen[id] = true
					byKey[id] = append(byKey[id], hostkey.PolicyRef{ID: p.Meta.ID, Name: p.Meta.Name})
				}
			}
			for _, pid := range p.Spec.PoolIDs {
				for _, id := range poolKeys[pid] {
					link(id)
				}
			}
			for _, id := range p.Spec.CredentialIDs {
				link(id)
			}
		}
		for _, k := range keys {
			k.Policies = byKey[k.Meta.ID]
		}
	}
}
