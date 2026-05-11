// Package limit implements a sliding-window-counter rate limiter with a
// two-phase Reserve/Commit API. Three meters are supported: requests,
// tokens, concurrency. State is persisted via pkg/state using a single
// RunScript call per phase (Lua on Redis, Go-emulator on MemStore).
package ratelimit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/internal/usage"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/reqid"
)

const (
	commitGuardTTL = 5 * time.Minute
)

// Limiter enforces rate-limit rules using pkg/state for counters.
// One Limiter is shared across the process; concurrent calls are safe.
type Limiter struct {
	runner kv.Scripter
	store  kv.Store
	log    *slog.Logger
	clock  func() time.Time
}

func New(s kv.Store, log *slog.Logger, clock func() time.Time) *Limiter {
	if clock == nil {
		clock = time.Now
	}
	l := &Limiter{store: s, log: log, clock: clock}
	if sr, ok := s.(kv.Scripter); ok {
		l.runner = sr
	}
	if ms, ok := s.(*kv.Mem); ok {
		RegisterScripts(ms)
	}
	return l
}

// Reservation is returned by a successful Reserve call.
type Reservation struct {
	ID       string
	poolName string // Redis Cluster hash-tag anchor; all keys share {policy:<poolName>}
	rules    []catalog.ResolvedRule
	// conKeys holds the concurrency state keys incremented at Reserve time.
	// Used by Commit to decrement them.
	conKeys []string
	// tokRules holds token-meter rules (for post-hoc Commit increment).
	tokRules []catalog.ResolvedRule
}

// Observations are passed to Commit to supply post-hoc measurements.
// Tokens is the typed map from PR 1 (usage.Tokens). Legacy callers can
// pass nil; per-meter rules will increment by zero in that case.
type Observations struct {
	Tokens    usage.Tokens
	Cancelled bool
}

// ExceededError is returned by Reserve when a budget is violated.
type ExceededError struct {
	Rule       catalog.ResolvedRule
	RetryAfter time.Duration
}

func (e *ExceededError) Error() string {
	return fmt.Sprintf("limit: budget exceeded: %s/%s/%s retry_after=%.0fs",
		e.Rule.ParentKind, e.Rule.ParentName, e.Rule.RateLimit.Metadata.Name,
		e.RetryAfter.Seconds())
}

func (e *ExceededError) Unwrap() error { return ErrExceeded }

var ErrExceeded = errors.New("limit: budget exceeded")

// Reserve checks all rules and increments counters atomically via one RunScript call.
// On violation, all increments from this call are rolled back and *ExceededError is returned.
// poolName is the policy that anchors all Redis Cluster hash tags for this call; pass an
// empty string only in tests or non-Cluster deployments.
func (l *Limiter) Reserve(ctx context.Context, poolName string, rules []catalog.ResolvedRule) (*Reservation, error) {
	now := l.clock()
	token := reqid.Generate()

	keys, ruleArgs, err := buildReserveArgs(poolName, rules, now)
	if err != nil {
		return nil, err
	}

	rulesJSON, err := json.Marshal(ruleArgs)
	if err != nil {
		return nil, fmt.Errorf("limit: marshal rules: %w", err)
	}

	raw, err := l.runner.RunScript(ctx, "limit.reserve", reserveLuaScript, keys,
		now.UnixMilli(), string(rulesJSON), token)
	if err != nil {
		return nil, fmt.Errorf("limit: reserve script: %w", err)
	}

	var res reserveResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("limit: decode reserve result: %w", err)
	}

	if res.Exceeded {
		exceeded := &ExceededError{
			RetryAfter: time.Duration(res.RetryAfterMs) * time.Millisecond,
		}
		// Reconstruct the violated rule from the result metadata.
		exceeded.Rule = findRule(rules, res.ParentKind, res.ParentName, res.RuleName, res.Meter)
		l.log.Info("limit reserve exceeded",
			"request_id", reqid.From(ctx),
			"rule", fmt.Sprintf("%s:%s:%s", res.ParentName, res.RuleName, res.Meter),
			"retry_after_seconds", exceeded.RetryAfter.Seconds(),
		)
		return nil, exceeded
	}

	reservation := &Reservation{
		ID:       token,
		poolName: poolName,
		rules:    rules,
	}
	// Pre-compute concurrency key list and token rule list for Commit.
	for _, rule := range rules {
		m := resolvedMeter(rule)
		switch {
		case m == string(catalog.MeterConcurrency):
			reservation.conKeys = append(reservation.conKeys, concurrencyKey(poolName, rule))
		case m == string(catalog.MeterTokens) || strings.HasPrefix(m, "tokens."):
			reservation.tokRules = append(reservation.tokRules, rule)
		}
	}

	l.log.Debug("limit reserve", "request_id", reqid.From(ctx), "rules", len(rules), "decision", "granted")
	return reservation, nil
}

// Commit finalizes a Reservation via one RunScript call. Tokens are incremented
// post-hoc; concurrency is always decremented. Calling Commit twice is a no-op.
//
// obs.Tokens is a typed map (usage.Tokens). Per-meter increments are derived as:
//   - meter "tokens":        obs.Tokens.Sum()  (all keys summed — backward compat)
//   - meter "tokens.<key>":  obs.Tokens["<key>"]  (specific key)
//   - meter "requests":      always 1 (counted at Reserve; not post-hoc)
//   - meter "concurrency":   decremented (not incremented)
func (l *Limiter) Commit(ctx context.Context, res *Reservation, obs Observations) error {
	now := l.clock()
	guardKey := commitGuardKey(res.poolName, res.ID)
	guardTTLMs := commitGuardTTL.Milliseconds()

	// Build per-token-rule amounts, one entry per tokRule.
	tokAmounts := make([]int64, len(res.tokRules))
	if !obs.Cancelled {
		for i, rule := range res.tokRules {
			m := resolvedMeter(rule)
			if m == string(catalog.MeterTokens) {
				tokAmounts[i] = obs.Tokens.Sum()
			} else if strings.HasPrefix(m, "tokens.") {
				key := m[len("tokens."):]
				tokAmounts[i] = obs.Tokens[key]
			}
		}
	}

	// Build KEYS: [guardKey, ...conKeys, ...tokCurKeys]
	keys := make([]string, 0, 1+len(res.conKeys)+len(res.tokRules))
	keys = append(keys, guardKey)
	keys = append(keys, res.conKeys...)

	var tokTTLMs int64
	for _, rule := range res.tokRules {
		w := rule.Window
		if w == 0 && rule.RateLimit != nil {
			w = rule.RateLimit.Spec.Window
		}
		cur, _ := windowBuckets(now, w)
		keys = append(keys, bucketKey(res.poolName, rule, cur))
		if ttl := (2 * w).Milliseconds(); ttl > tokTTLMs {
			tokTTLMs = ttl
		}
	}

	// Encode per-rule token amounts as JSON array.
	tokAmountsJSON, err := json.Marshal(tokAmounts)
	if err != nil {
		return fmt.Errorf("limit: marshal tok_amounts: %w", err)
	}

	raw, err := l.runner.RunScript(ctx, "limit.commit", commitLuaScript, keys,
		res.ID,
		guardTTLMs,
		int64(len(res.conKeys)),
		int64(len(res.tokRules)),
		string(tokAmountsJSON),
		tokTTLMs,
	)
	if err != nil {
		return fmt.Errorf("limit: commit script: %w", err)
	}

	if string(raw) == "noop" {
		l.log.Debug("limit commit duplicate", "reservation_id", res.ID)
		return nil
	}

	l.log.Debug("limit commit",
		"reservation_id", res.ID,
		"tokens_sum", obs.Tokens.Sum(),
		"cancelled", obs.Cancelled,
	)
	return nil
}

// RemainingByMeter returns the smallest remaining capacity per meter across rules.
// poolName must match the value used in Reserve for the same rules.
func (l *Limiter) RemainingByMeter(ctx context.Context, poolName string, rules []catalog.ResolvedRule) (map[catalog.Meter]int64, error) {
	now := l.clock()
	result := make(map[catalog.Meter]int64)

	for _, rule := range rules {
		w := rule.Window
		if w == 0 && rule.RateLimit != nil {
			w = rule.RateLimit.Spec.Window
		}
		amount := rule.Rule.Amount
		if amount == 0 && rule.RateLimit != nil {
			amount = rule.RateLimit.Spec.Amount
		}
		cur, prev := windowBuckets(now, w)
		frac := fractionElapsed(now, cur, w)

		m := catalog.Meter(resolvedMeter(rule))
		var rate float64

		switch {
		case m == catalog.MeterConcurrency:
			cVal, err := readCounter(ctx, l.store, concurrencyKey(poolName, rule))
			if err != nil {
				return nil, err
			}
			rate = float64(cVal)
		default:
			// requests, tokens, tokens.X — sliding window
			curKey := bucketKey(poolName, rule, cur)
			prevKey := bucketKey(poolName, rule, prev)
			curVal, err := readCounter(ctx, l.store, curKey)
			if err != nil {
				return nil, err
			}
			prevVal, err := readCounter(ctx, l.store, prevKey)
			if err != nil {
				return nil, err
			}
			rate = interpolatedRate(curVal, prevVal, frac)
		}

		remaining := amount - int64(rate)
		if remaining < 0 {
			remaining = 0
		}

		if existing, ok := result[m]; !ok || remaining < existing {
			result[m] = remaining
		}
	}

	return result, nil
}

// findRule looks up a rule by its identifying fields; returns a zero-value
// ResolvedRule with synthesized fields if not found (avoids nil panic).
func findRule(rules []catalog.ResolvedRule, parentKind, parentName, ruleName, meter string) catalog.ResolvedRule {
	for _, r := range rules {
		rlName := r.RateLimitName
		if rlName == "" && r.RateLimit != nil {
			rlName = r.RateLimit.Metadata.Name
		}
		rm := resolvedMeter(r)
		if string(r.ParentKind) == parentKind &&
			r.ParentName == parentName &&
			rlName == ruleName &&
			rm == meter {
			return r
		}
	}
	// Fallback: synthesize a minimal rule so the error message is useful.
	return catalog.ResolvedRule{
		ParentKind:    catalog.Kind(parentKind),
		ParentName:    parentName,
		RateLimitName: ruleName,
		Meter:         catalog.Meter(meter),
		Rule:          catalog.RateLimitRule{Meter: meter},
		RateLimit: &catalog.RateLimit{
			Metadata: catalog.Metadata{Name: ruleName},
		},
	}
}
