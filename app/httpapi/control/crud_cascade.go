package control

import (
	"context"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/ratelimit"
)

// cascadeHostKeyDetach strips the deleted HostKey from every policy that
// names it (see policy.DetachHostKey). Shared with apply's prune.
func cascadeHostKeyDetach(d Deps) cascadeFn[hostkey.HostKey] {
	return func(ctx context.Context, k *hostkey.HostKey) error {
		if k == nil || d.Stores == nil || d.Stores.Policy == nil {
			return nil
		}
		return policy.DetachHostKey(ctx, d.Stores.Policy, k.Meta.ID)
	}
}

// detachStores adapts Deps to the write surface app/policy.Detach needs.
func detachStores(d Deps) policy.DetachStores {
	var s policy.DetachStores
	if d.Stores == nil {
		return s
	}
	if d.Stores.Key != nil {
		s.Keys = d.Stores.Key
	}
	if d.Stores.ServiceAccount != nil {
		s.ServiceAccounts = d.Stores.ServiceAccount
	}
	if d.Stores.HostKey != nil {
		s.HostKeys = d.Stores.HostKey
	}
	if d.Stores.Host != nil {
		s.Hosts = d.Stores.Host
	}
	return s
}

// cascadePolicyDetach scrubs every stored reference to the deleted Policy
// before the row is removed. Shared with apply's prune so both removal paths
// leave the same state behind.
func cascadePolicyDetach(d Deps) cascadeFn[policy.Policy] {
	return func(ctx context.Context, p *policy.Policy) error {
		if p == nil || d.Stores == nil {
			return nil
		}
		return policy.Detach(ctx, detachStores(d), p.Meta.ID)
	}
}

// cascadeRateLimitDetach strips the deleted RateLimit from every policy's
// RLBindings (see policy.DetachRateLimit). Shared with apply's prune.
func cascadeRateLimitDetach(d Deps) cascadeFn[ratelimit.RateLimit] {
	return func(ctx context.Context, r *ratelimit.RateLimit) error {
		if r == nil || d.Stores == nil || d.Stores.Policy == nil {
			return nil
		}
		return policy.DetachRateLimit(ctx, d.Stores.Policy, r.Meta.ID)
	}
}
