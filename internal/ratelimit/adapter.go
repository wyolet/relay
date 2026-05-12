// Package ratelimit is a thin adapter that translates Relay catalog types to
// pkg/ratelimit.Rule and delegates all rate-limit logic to that package.
//
// Public surface is preserved for existing callers:
//   - Limiter, Reservation, ExceededError, ErrExceeded, Observations, New
//   - l.Reserve(ctx, poolName, []catalog.ResolvedRule)
//   - l.Commit(ctx, *Reservation, Observations)
//   - RegisterScripts(*kv.Mem)
//
// The Limiter is a wrapper struct (not a type alias) because the Reserve/Commit
// methods need to accept catalog.ResolvedRule rather than pkg.Rule.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/internal/usage"
	pkgrl "github.com/wyolet/relay/pkg/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
)

// RegisterScripts re-exports pkg.RegisterScripts for callers that construct
// kv.Mem stores and need to pre-register the Lua emulators.
var RegisterScripts = pkgrl.RegisterScripts

// ErrExceeded is re-exported so callers can use errors.Is(err, ratelimit.ErrExceeded).
var ErrExceeded = pkgrl.ErrExceeded

// KeyQuotaExhausted wraps pkgrl.KeyQuotaExhausted and adds a catalog.ResolvedRule
// field so existing callers that do ee.Rule.RateLimit.Metadata.Name keep working.
// It is used for both policy-level (pre-Pick) and per-key (post-Pick) Reserve
// failures; the call site determines which scope was exhausted.
type KeyQuotaExhausted struct {
	Rule       catalog.ResolvedRule
	RetryAfter time.Duration
	// pkg is the underlying pkg error (for Unwrap).
	pkg *pkgrl.KeyQuotaExhausted
}

func (e *KeyQuotaExhausted) Error() string {
	return fmt.Sprintf("limit: budget exceeded: %s/%s/%s retry_after=%.0fs",
		e.Rule.ParentKind, e.Rule.ParentName, e.Rule.RateLimit.Metadata.Name,
		e.RetryAfter.Seconds())
}

func (e *KeyQuotaExhausted) Unwrap() error { return ErrExceeded }

// ExceededError is an alias kept for backward compatibility with callers that
// use the old name. New code should use KeyQuotaExhausted directly.
//
// Deprecated: Use KeyQuotaExhausted.
type ExceededError = KeyQuotaExhausted

// Reservation wraps pkgrl.Reservation together with the original catalog rules
// so Commit can reconstruct token amounts from usage.Tokens.
type Reservation struct {
	inner *pkgrl.Reservation
	rules []catalog.ResolvedRule
}

// Observations holds post-hoc measurements. Tokens is usage.Tokens (map[string]int64).
type Observations struct {
	Tokens    usage.Tokens
	Cancelled bool
}

// Limiter wraps pkgrl.Limiter and translates catalog types.
type Limiter struct {
	inner *pkgrl.Limiter
	store kv.Store
	clock func() time.Time
}

// New creates a Limiter backed by pkg/ratelimit.
func New(s kv.Store, log *slog.Logger, clock func() time.Time) *Limiter {
	if clock == nil {
		clock = time.Now
	}
	return &Limiter{
		inner: pkgrl.New(s, log, clock),
		store: s,
		clock: clock,
	}
}

// Reserve translates []catalog.ResolvedRule to []pkg.Rule and calls pkg Reserve.
// poolName is used as the scope with prefix "policy:" so the Redis key format
// "limit:{policy:<poolName>}:..." is preserved exactly.
func (l *Limiter) Reserve(ctx context.Context, poolName string, rules []catalog.ResolvedRule) (*Reservation, error) {
	scope := "policy:" + poolName
	pkgRules := toRules(rules)

	inner, err := l.inner.Reserve(ctx, scope, pkgRules)
	if err != nil {
		var pe *pkgrl.KeyQuotaExhausted
		if errors.As(err, &pe) {
			// Reconstruct the catalog rule for the violated rule.
			catalogRule := findCatalogRule(rules, pe.Rule.Key, pe.Rule.Meter)
			return nil, &KeyQuotaExhausted{
				Rule:       catalogRule,
				RetryAfter: pe.RetryAfter,
				pkg:        pe,
			}
		}
		return nil, err
	}
	return &Reservation{inner: inner, rules: rules}, nil
}

// ReserveSecret is like Reserve but uses scope "secret:<keyHash>" instead of
// "policy:<poolName>". It is called after keypool.Pick to enforce per-key rules
// (secret-attached rate limits). On KeyQuotaExhausted the caller should invoke
// keypool.Selector.RecordLocalRateLimit to cool down the chosen key.
func (l *Limiter) ReserveSecret(ctx context.Context, keyHash string, rules []catalog.ResolvedRule) (*Reservation, error) {
	scope := "secret:" + keyHash
	pkgRules := toRules(rules)

	inner, err := l.inner.Reserve(ctx, scope, pkgRules)
	if err != nil {
		var pe *pkgrl.KeyQuotaExhausted
		if errors.As(err, &pe) {
			catalogRule := findCatalogRule(rules, pe.Rule.Key, pe.Rule.Meter)
			return nil, &KeyQuotaExhausted{
				Rule:       catalogRule,
				RetryAfter: pe.RetryAfter,
				pkg:        pe,
			}
		}
		return nil, err
	}
	return &Reservation{inner: inner, rules: rules}, nil
}

// Commit finalizes a Reservation.
func (l *Limiter) Commit(ctx context.Context, res *Reservation, obs Observations) error {
	pkgObs := pkgrl.Observations{
		Tokens:    map[string]int64(obs.Tokens),
		Cancelled: obs.Cancelled,
	}
	return l.inner.Commit(ctx, res.inner, pkgObs)
}

// toRules translates []catalog.ResolvedRule to []pkgrl.Rule.
// Rule.Key encodes (parentKind, parentName, rlName) so it matches the key
// format used in the old internal keys.go functions.
func toRules(in []catalog.ResolvedRule) []pkgrl.Rule {
	out := make([]pkgrl.Rule, len(in))
	for i, r := range in {
		rlName := resolvedRLName(r)
		meter := resolvedMeter(r)
		out[i] = pkgrl.Rule{
			Key:      fmt.Sprintf("%s:%s:%s", r.ParentKind, r.ParentName, rlName),
			Name:     rlName + ":" + meter,
			Meter:    meter,
			Strategy: pkgrl.Strategy(resolvedStrategy(r)),
			Amount:   r.Rule.Amount,
			Window:   resolvedWindow(r),
		}
	}
	return out
}

// findCatalogRule reconstructs a catalog.ResolvedRule from an ExceededError's
// identifiers. Returns a synthesized rule if no match.
func findCatalogRule(rules []catalog.ResolvedRule, ruleKey, meter string) catalog.ResolvedRule {
	for _, r := range rules {
		rlName := resolvedRLName(r)
		key := fmt.Sprintf("%s:%s:%s", r.ParentKind, r.ParentName, rlName)
		if key == ruleKey && resolvedMeter(r) == meter {
			return r
		}
	}
	// Parse ruleKey back to components if possible: "Kind:name:rlName"
	// for synthesized error messages.
	rlName := ""
	parts := strings.SplitN(ruleKey, ":", 3)
	if len(parts) == 3 {
		rlName = parts[2]
	}
	return catalog.ResolvedRule{
		Rule:  catalog.RateLimitRule{Meter: meter},
		Meter: catalog.Meter(meter),
		RateLimit: &catalog.RateLimit{
			Metadata: catalog.Metadata{Name: rlName},
		},
	}
}

// resolvedStrategy returns the effective strategy for a ResolvedRule.
func resolvedStrategy(r catalog.ResolvedRule) catalog.RateLimitStrategy {
	if r.Strategy != "" {
		return r.Strategy
	}
	if r.RateLimit != nil && r.RateLimit.Spec.Strategy != "" {
		return r.RateLimit.Spec.Strategy
	}
	return catalog.StrategyTokenBucket
}

// resolvedMeter returns the effective meter string for a ResolvedRule.
func resolvedMeter(r catalog.ResolvedRule) string {
	if r.Rule.Meter != "" {
		return r.Rule.Meter
	}
	return string(r.Meter)
}

// resolvedRLName returns the effective RateLimit name for a ResolvedRule.
func resolvedRLName(r catalog.ResolvedRule) string {
	if r.RateLimitName != "" {
		return r.RateLimitName
	}
	if r.RateLimit != nil {
		return r.RateLimit.Metadata.Name
	}
	return ""
}

// resolvedWindow returns the effective window for a ResolvedRule.
func resolvedWindow(r catalog.ResolvedRule) time.Duration {
	if r.Window != 0 {
		return r.Window
	}
	if r.RateLimit != nil {
		return r.RateLimit.Spec.Window
	}
	return 0
}
