package control

import (
	"context"
	"errors"

	"github.com/danielgtaylor/huma/v2"

	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// MintLimiter meters how often one user may mint a token. Satisfied by
// *pkg/ratelimit.Limiter; nil leaves minting unmetered.
type MintLimiter interface {
	Reserve(ctx context.Context, scope string, rules []pkgratelimit.Rule) (*pkgratelimit.Reservation, error)
}

// reserveMint meters one mint against the caller's fixed window. A limiter
// outage must not take minting down, so only an explicit budget violation
// is fatal.
func reserveMint(ctx context.Context, d tokenDeps, userID string) error {
	if d.limiter == nil {
		return nil
	}
	_, err := d.limiter.Reserve(ctx, mintLimitScope(userID), []pkgratelimit.Rule{{
		Key:      mintLimitRule,
		Name:     "token mints",
		Meter:    "requests",
		Strategy: pkgratelimit.StrategyFixedWindow,
		Amount:   mintLimitBudget,
		Window:   mintLimitWindow,
	}})
	if errors.Is(err, pkgratelimit.ErrExceeded) {
		return huma.Error429TooManyRequests("too many token mints; retry shortly")
	}
	return nil
}
