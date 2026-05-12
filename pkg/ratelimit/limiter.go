package ratelimit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/reqid"
)

const (
	commitGuardTTL = 5 * time.Minute
)

// Limiter enforces rate-limit rules using pkg/kv for counters.
// One Limiter is shared across the process; concurrent calls are safe.
type Limiter struct {
	runner kv.Scripter
	store  kv.Store
	log    *slog.Logger
	clock  func() time.Time
}

// New creates a Limiter. If clock is nil, time.Now is used.
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
	ID    string
	scope string // Redis Cluster hash-tag anchor; all keys share {<scope>}
	rules []Rule
	// conKeys holds the concurrency state keys incremented at Reserve time.
	conKeys []string
	// tokRules holds token-meter rules (for post-hoc Commit increment).
	tokRules []Rule
	// tbRules holds token-bucket rules that need state-key refund on cancel.
	tbRules []Rule
	// lbRules holds leaky-bucket rules that need state-key refund on cancel.
	lbRules []Rule
	// swRules holds session-window rules that need count refund on cancel.
	swRules []Rule
}

// Reserve checks all rules and increments counters atomically via one RunScript call.
// On violation, all increments from this call are rolled back and *ExceededError is returned.
// scope is the Redis Cluster hash-tag anchor (e.g. "policy:prod-policy").
func (l *Limiter) Reserve(ctx context.Context, scope string, rules []Rule) (*Reservation, error) {
	now := l.clock()
	token := reqid.Generate()

	keys, ruleArgs, err := buildReserveArgs(scope, rules, now)
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
		exceeded.Rule = findRule(rules, res.RuleKey, res.RuleName, res.Meter)
		l.log.Info("limit reserve exceeded",
			"request_id", reqid.From(ctx),
			"rule", res.RuleName,
			"meter", res.Meter,
			"retry_after_seconds", exceeded.RetryAfter.Seconds(),
		)
		return nil, exceeded
	}

	reservation := &Reservation{
		ID:    token,
		scope: scope,
		rules: rules,
	}
	// Pre-compute key/rule lists for Commit.
	for _, rule := range rules {
		switch {
		case rule.Meter == "concurrency":
			reservation.conKeys = append(reservation.conKeys, concurrencyKey(scope, rule))
		case rule.Meter == "tokens" || strings.HasPrefix(rule.Meter, "tokens."):
			reservation.tokRules = append(reservation.tokRules, rule)
		case rule.Strategy == StrategyTokenBucket:
			reservation.tbRules = append(reservation.tbRules, rule)
		case rule.Strategy == StrategyLeakyBucket:
			reservation.lbRules = append(reservation.lbRules, rule)
		case rule.Strategy == StrategySessionWindow:
			reservation.swRules = append(reservation.swRules, rule)
		}
	}

	l.log.Debug("limit reserve", "request_id", reqid.From(ctx), "rules", len(rules), "decision", "granted")
	return reservation, nil
}

// Commit finalizes a Reservation via one RunScript call. Tokens are incremented
// post-hoc; concurrency is always decremented. Calling Commit twice is a no-op.
//
// obs.Tokens is a map[string]int64. Per-meter increments are derived as:
//   - meter "tokens":        sum of all values in obs.Tokens (backward compat)
//   - meter "tokens.<key>":  obs.Tokens["<key>"]
//   - meter "requests":      always 1 (counted at Reserve; not post-hoc)
//   - meter "concurrency":   decremented (not incremented)
func (l *Limiter) Commit(ctx context.Context, res *Reservation, obs Observations) error {
	now := l.clock()
	guardKey := commitGuardKey(res.scope, res.ID)
	guardTTLMs := commitGuardTTL.Milliseconds()

	// Build per-token-rule amounts, one entry per tokRule.
	tokAmounts := make([]int64, len(res.tokRules))
	if !obs.Cancelled {
		for i, rule := range res.tokRules {
			m := rule.Meter
			if m == "tokens" {
				// sum all values
				var sum int64
				for _, v := range obs.Tokens {
					sum += v
				}
				tokAmounts[i] = sum
			} else if strings.HasPrefix(m, "tokens.") {
				key := m[len("tokens."):]
				tokAmounts[i] = obs.Tokens[key]
			}
		}
	}

	// Build KEYS: [guardKey, ...conKeys, ...tokCurKeys, ...tbStateKeys, ...lbStateKeys, ...swStateKeys]
	keys := make([]string, 0, 1+len(res.conKeys)+len(res.tokRules)+len(res.tbRules)+len(res.lbRules)+len(res.swRules))
	keys = append(keys, guardKey)
	keys = append(keys, res.conKeys...)

	var tokTTLMs int64
	for _, rule := range res.tokRules {
		w := rule.Window
		cur, _ := windowBuckets(now, w)
		keys = append(keys, bucketKey(res.scope, rule, cur))
		if ttl := (2 * w).Milliseconds(); ttl > tokTTLMs {
			tokTTLMs = ttl
		}
	}

	// Append tb/lb/sw state keys and build refund descriptors (1-based KEYS indices).
	type refund3 [3]int64 // [key_idx, cost_scaled, burst]
	type refund2 [2]int64 // [key_idx, cost_scaled]
	tbRefunds := make([]refund3, 0, len(res.tbRules))
	for _, rule := range res.tbRules {
		keys = append(keys, tbStateKey(res.scope, rule))
		keyIdx := int64(len(keys)) // 1-based
		tbRefunds = append(tbRefunds, refund3{keyIdx, 1000, rule.Amount})
	}
	lbRefunds := make([]refund2, 0, len(res.lbRules))
	for _, rule := range res.lbRules {
		keys = append(keys, lbStateKey(res.scope, rule))
		keyIdx := int64(len(keys)) // 1-based
		lbRefunds = append(lbRefunds, refund2{keyIdx, 1000})
	}
	swRefunds := make([]refund2, 0, len(res.swRules))
	for _, rule := range res.swRules {
		keys = append(keys, swStateKey(res.scope, rule))
		keyIdx := int64(len(keys)) // 1-based
		swRefunds = append(swRefunds, refund2{keyIdx, 1})
	}

	// Encode per-rule token amounts as JSON array.
	tokAmountsJSON, err := json.Marshal(tokAmounts)
	if err != nil {
		return fmt.Errorf("limit: marshal tok_amounts: %w", err)
	}
	tbRefundsJSON, err := json.Marshal(tbRefunds)
	if err != nil {
		return fmt.Errorf("limit: marshal tb_refunds: %w", err)
	}
	lbRefundsJSON, err := json.Marshal(lbRefunds)
	if err != nil {
		return fmt.Errorf("limit: marshal lb_refunds: %w", err)
	}
	swRefundsJSON, err := json.Marshal(swRefunds)
	if err != nil {
		return fmt.Errorf("limit: marshal sw_refunds: %w", err)
	}

	cancelledInt := int64(0)
	if obs.Cancelled {
		cancelledInt = 1
	}

	raw, err := l.runner.RunScript(ctx, "limit.commit", commitLuaScript, keys,
		res.ID,
		guardTTLMs,
		int64(len(res.conKeys)),
		int64(len(res.tokRules)),
		string(tokAmountsJSON),
		tokTTLMs,
		cancelledInt,
		string(tbRefundsJSON),
		string(lbRefundsJSON),
		string(swRefundsJSON),
	)
	if err != nil {
		return fmt.Errorf("limit: commit script: %w", err)
	}

	if string(raw) == "noop" {
		l.log.Debug("limit commit duplicate", "reservation_id", res.ID)
		return nil
	}

	tokSum := int64(0)
	for _, v := range obs.Tokens {
		tokSum += v
	}
	l.log.Debug("limit commit",
		"reservation_id", res.ID,
		"tokens_sum", tokSum,
		"cancelled", obs.Cancelled,
	)
	return nil
}

// findRule looks up a rule by its identifying fields; returns a synthesized Rule
// if not found (avoids nil issues in error paths).
func findRule(rules []Rule, ruleKey, ruleName, meter string) Rule {
	for _, r := range rules {
		if r.Key == ruleKey && r.Meter == meter {
			return r
		}
	}
	// Fallback: synthesize a minimal rule for error messages.
	return Rule{
		Key:   ruleKey,
		Name:  ruleName,
		Meter: meter,
	}
}
