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
