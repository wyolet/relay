// Package limit implements a sliding-window-counter rate limiter with a
// two-phase Reserve/Commit API. Three meters are supported: requests,
// tokens, concurrency. State is persisted via pkg/state using a single
// RunScript call per phase (Lua on Redis, Go-emulator on MemStore).
package limit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/wyolet/relay/pkg/configstore"
	"github.com/wyolet/relay/pkg/reqid"
	"github.com/wyolet/relay/pkg/kv"
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
	poolName string // Redis Cluster hash-tag anchor; all keys share {pool:<poolName>}
	rules    []configstore.ResolvedRule
	// conKeys holds the concurrency state keys incremented at Reserve time.
	// Used by Commit to decrement them.
	conKeys []string
	// tokRules holds token-meter rules (for post-hoc Commit increment).
	tokRules []configstore.ResolvedRule
}

// Observations are passed to Commit to supply post-hoc measurements.
type Observations struct {
	Tokens    int64
	Cancelled bool
}

// ExceededError is returned by Reserve when a budget is violated.
type ExceededError struct {
	Rule       configstore.ResolvedRule
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
// poolName is the pool that anchors all Redis Cluster hash tags for this call; pass an
// empty string only in tests or non-Cluster deployments.
func (l *Limiter) Reserve(ctx context.Context, poolName string, rules []configstore.ResolvedRule) (*Reservation, error) {
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
		switch rule.Meter {
		case configstore.MeterConcurrency:
			reservation.conKeys = append(reservation.conKeys, concurrencyKey(poolName, rule))
		case configstore.MeterTokens:
			reservation.tokRules = append(reservation.tokRules, rule)
		}
	}

	l.log.Debug("limit reserve", "request_id", reqid.From(ctx), "rules", len(rules), "decision", "granted")
	return reservation, nil
}

// Commit finalizes a Reservation via one RunScript call. Tokens are incremented
// post-hoc; concurrency is always decremented. Calling Commit twice is a no-op.
func (l *Limiter) Commit(ctx context.Context, res *Reservation, obs Observations) error {
	now := l.clock()
	guardKey := commitGuardKey(res.poolName, res.ID)
	guardTTLMs := commitGuardTTL.Milliseconds()

	// Build KEYS: [guardKey, ...conKeys, ...tokCurKeys]
	keys := make([]string, 0, 1+len(res.conKeys)+len(res.tokRules))
	keys = append(keys, guardKey)
	keys = append(keys, res.conKeys...)

	var tokTTLMs int64
	for _, rule := range res.tokRules {
		w := rule.RateLimit.Spec.Window
		cur, _ := windowBuckets(now, w)
		keys = append(keys, bucketKey(res.poolName, rule, cur))
		if ttl := (2 * w).Milliseconds(); ttl > tokTTLMs {
			tokTTLMs = ttl
		}
	}

	actualTok := obs.Tokens
	if obs.Cancelled {
		actualTok = 0
	}

	raw, err := l.runner.RunScript(ctx, "limit.commit", commitLuaScript, keys,
		res.ID,
		guardTTLMs,
		int64(len(res.conKeys)),
		int64(len(res.tokRules)),
		actualTok,
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
		"tokens", obs.Tokens,
		"cancelled", obs.Cancelled,
	)
	return nil
}

// RemainingByMeter returns the smallest remaining capacity per meter across rules.
// poolName must match the value used in Reserve for the same rules.
func (l *Limiter) RemainingByMeter(ctx context.Context, poolName string, rules []configstore.ResolvedRule) (map[configstore.Meter]int64, error) {
	now := l.clock()
	result := make(map[configstore.Meter]int64)

	for _, rule := range rules {
		w := rule.RateLimit.Spec.Window
		amount := rule.RateLimit.Spec.Amount
		cur, prev := windowBuckets(now, w)
		frac := fractionElapsed(now, cur, w)

		var rate float64

		switch rule.Meter {
		case configstore.MeterRequests, configstore.MeterTokens:
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

		case configstore.MeterConcurrency:
			cVal, err := readCounter(ctx, l.store, concurrencyKey(poolName, rule))
			if err != nil {
				return nil, err
			}
			rate = float64(cVal)
		}

		remaining := amount - int64(rate)
		if remaining < 0 {
			remaining = 0
		}

		if existing, ok := result[rule.Meter]; !ok || remaining < existing {
			result[rule.Meter] = remaining
		}
	}

	return result, nil
}

// findRule looks up a rule by its identifying fields; returns a zero-value
// ResolvedRule with synthesized RateLimit if not found (avoids nil panic).
func findRule(rules []configstore.ResolvedRule, parentKind, parentName, ruleName, meter string) configstore.ResolvedRule {
	for _, r := range rules {
		if string(r.ParentKind) == parentKind &&
			r.ParentName == parentName &&
			r.RateLimit.Metadata.Name == ruleName &&
			string(r.Meter) == meter {
			return r
		}
	}
	// Fallback: synthesize a minimal rule so the error message is useful.
	return configstore.ResolvedRule{
		ParentKind: configstore.Kind(parentKind),
		ParentName: parentName,
		Meter:      configstore.Meter(meter),
		RateLimit: &configstore.RateLimit{
			Metadata: configstore.Metadata{Name: ruleName},
		},
	}
}
