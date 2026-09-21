// detach.go scrubs the references to a Policy that live in other rows' spec
// JSONB. Postgres FKs only cover the join tables (policy_models,
// policy_host_keys, both CASCADE); everything below needs app-level cleanup,
// and both writers that can remove a policy — the control API's delete
// cascade and apply's prune — must do exactly the same one.
package policy

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/serviceaccount"
)

// DetachStores is the narrow store surface Detach writes through. A nil field
// skips that kind, so a caller that has not wired a store is not forced to.
type DetachStores struct {
	Keys interface {
		List(context.Context) ([]*key.Key, error)
		Upsert(context.Context, *key.Key) error
	}
	ServiceAccounts interface {
		List(context.Context) ([]*serviceaccount.ServiceAccount, error)
		Upsert(context.Context, *serviceaccount.ServiceAccount) error
	}
	// HostKeys is read-only: it answers HostKeysUsingPolicy, which decides
	// whether the policy may be removed at all.
	HostKeys interface {
		List(context.Context) ([]*hostkey.HostKey, error)
	}
	Hosts interface {
		List(context.Context) ([]*host.Host, error)
		Upsert(context.Context, *host.Host) error
	}
}

// Detach clears every stored reference to the policy id:
//   - Key.spec.policyId — the key becomes policy-less and follows
//     settings.Inference.AllowMissingPolicy on the hot path.
//   - ServiceAccount.spec.policyId — the account falls back to its project's
//     policy bindings.
//   - Host.spec.policies[] entries and Host.spec.defaultPolicy.
//
// HostKey.spec.policyId is deliberately NOT cleared: policyId is required, so
// clearing it writes a row that fails its own Validate. A policy host keys
// mirror as their tier is refused deletion instead — see HostKeysUsingPolicy
// (D76, D81).
func Detach(ctx context.Context, s DetachStores, id string) error {
	if id == "" {
		return nil
	}
	if s.Keys != nil {
		rows, err := s.Keys.List(ctx)
		if err != nil {
			return fmt.Errorf("list keys: %w", err)
		}
		for _, k := range rows {
			if k.Spec.PolicyID != id {
				continue
			}
			k.Spec.PolicyID = ""
			if err := s.Keys.Upsert(ctx, k); err != nil {
				return fmt.Errorf("detach from key %q: %w", k.Meta.Name, err)
			}
		}
	}
	if s.ServiceAccounts != nil {
		rows, err := s.ServiceAccounts.List(ctx)
		if err != nil {
			return fmt.Errorf("list service accounts: %w", err)
		}
		for _, sa := range rows {
			if sa.Spec.PolicyID != id {
				continue
			}
			sa.Spec.PolicyID = ""
			if err := s.ServiceAccounts.Upsert(ctx, sa); err != nil {
				return fmt.Errorf("detach from service account %q: %w", sa.Meta.Name, err)
			}
		}
	}
	if s.Hosts != nil {
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
				if changed {
					if len(filtered) == 0 {
						h.Spec.Policies = nil
					} else {
						h.Spec.Policies = filtered
					}
				}
			}
			if !changed {
				continue
			}
			if err := s.Hosts.Upsert(ctx, h); err != nil {
				return fmt.Errorf("detach from host %q: %w", h.Meta.Name, err)
			}
		}
	}
	return nil
}

// HostKeysUsingPolicy returns the host keys that name id as their tier
// policy. Deleting such a policy would leave every one of them invalid, so
// the delete is refused instead (D76).
func HostKeysUsingPolicy(ctx context.Context, s DetachStores, id string) ([]string, error) {
	if s.HostKeys == nil || id == "" {
		return nil, nil
	}
	rows, err := s.HostKeys.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list host-keys: %w", err)
	}
	var names []string
	for _, k := range rows {
		if k.Spec.PolicyID == id {
			names = append(names, k.Meta.Name)
		}
	}
	return names, nil
}
