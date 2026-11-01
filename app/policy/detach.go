// A host's policy menu lives in its spec JSONB, which Postgres FKs do not
// cover, so apply's prune clears it before removing a policy. Bindings of
// the policy go with it through their foreign key.

package policy

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/app/host"
)

// DetachStores is the narrow store surface Detach writes through. A nil field
// skips that kind, so a caller that has not wired a store is not forced to.
type DetachStores struct {
	Hosts interface {
		List(context.Context) ([]*host.Host, error)
		Upsert(context.Context, *host.Host) error
	}
}

// Detach clears every Host.spec.policies[] entry and Host.spec.defaultPolicy
// naming the policy id.
func Detach(ctx context.Context, s DetachStores, id string) error {
	if id == "" || s.Hosts == nil {
		return nil
	}
	rows, err := s.Hosts.List(ctx)
	if err != nil {
		return fmt.Errorf("list hosts: %w", err)
	}
	for _, h := range rows {
		changed := false
		if h.Spec.DefaultPolicy == id {
			h.Spec.DefaultPolicy = ""
			changed = true
		}
		if len(h.Spec.Policies) > 0 {
			filtered := make([]string, 0, len(h.Spec.Policies))
			for _, pid := range h.Spec.Policies {
				if pid == id {
					changed = true
					continue
				}
				filtered = append(filtered, pid)
			}
			if len(filtered) == 0 {
				filtered = nil
			}
			h.Spec.Policies = filtered
		}
		if !changed {
			continue
		}
		if err := s.Hosts.Upsert(ctx, h); err != nil {
			return fmt.Errorf("detach from host %q: %w", h.Meta.Name, err)
		}
	}
	return nil
}
