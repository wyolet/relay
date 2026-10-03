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
// with the user Policies that reference this key via Spec.HostKeyIDs,
// read off the current catalog snapshot. Reverse-ref summary for the
// admin UI; never persisted (the field is yaml:"-" and skipped by the
// store).
func enrichHostKeyPolicies(d Deps) enrichFn[hostkey.HostKey] {
	return func(ctx context.Context, k *hostkey.HostKey) {
		if k == nil || d.Stores == nil || d.Stores.Policy == nil {
			return
		}
		pols, err := d.Stores.Policy.List(ctx)
		if err != nil {
			return
		}
		var refs []hostkey.PolicyRef
		for _, p := range pols {
			for _, id := range p.Spec.HostKeyIDs {
				if id == k.Meta.ID {
					refs = append(refs, hostkey.PolicyRef{ID: p.Meta.ID, Name: p.Meta.Name})
					break
				}
			}
		}
		k.Policies = refs
	}
}

// enrichHostKeyPoliciesAll is the list-path variant: one Policy.List for the
// whole page instead of one per key row (the former N+1 on /api/host-keys).
func enrichHostKeyPoliciesAll(d Deps) enrichListFn[hostkey.HostKey] {
	return func(ctx context.Context, keys []*hostkey.HostKey) {
		if len(keys) == 0 || d.Stores == nil || d.Stores.Policy == nil {
			return
		}
		pols, err := d.Stores.Policy.List(ctx)
		if err != nil {
			return
		}
		byKey := map[string][]hostkey.PolicyRef{}
		for _, p := range pols {
			seen := map[string]bool{}
			for _, id := range p.Spec.HostKeyIDs {
				if seen[id] {
					continue
				}
				seen[id] = true
				byKey[id] = append(byKey[id], hostkey.PolicyRef{ID: p.Meta.ID, Name: p.Meta.Name})
			}
		}
		for _, k := range keys {
			k.Policies = byKey[k.Meta.ID]
		}
	}
}
