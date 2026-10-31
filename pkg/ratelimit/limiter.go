package ratelimit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/metrics"
	"github.com/wyolet/relay/pkg/reqid"
)

// commitGuardTTL outlasts the longest gap between two commits of one reservation: a commit script resent within one call, or a failed-over key's release and then the error-path commits, at most a few kv command bounds (about 45 s on the client defaults).
const commitGuardTTL = time.Minute

// Limiter enforces rate-limit rules using pkg/kv for counters.
// One Limiter is shared across the process; concurrent calls are safe.
type Limiter struct {
	runner kv.Scripter
	store  kv.Store
	log    *slog.Logger
	clock  func() time.Time

	// releaser frees the leases of reserves whose reply was lost (release_unanswered.go).
	releaser     kv.Scripter
	releaseSlots chan struct{}
	releases     sync.WaitGroup
	// shedRunner runs ReserveOrShed's script: the store's RunScriptOrShed when it has one, else runner.
	shedRunner kv.Scripter
}

// New creates a Limiter. If clock is nil, time.Now is used.
func New(s kv.Store, log *slog.Logger, clock func() time.Time) *Limiter {
	if clock == nil {
		clock = time.Now
	}
	l := &Limiter{store: s, log: log, clock: clock, releaseSlots: make(chan struct{}, maxUnansweredReleases)}
	if sr, ok := s.(kv.Scripter); ok {
		l.runner = sr
		l.releaser = sr
	}
	l.shedRunner = l.runner
	if ps, ok := s.(kv.PoolShedder); ok {
		l.shedRunner = shedScripter{ps}
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
	// conKeys holds the concurrency lease sets this reservation joined at Reserve time.
	conKeys []string
	// tokRules holds token-meter rules (for post-hoc Commit increment).
	tokRules []Rule
	// tbRules holds token-bucket rules that need state-key refund on cancel.
	tbRules []Rule
	// lbRules holds leaky-bucket rules that need state-key refund on cancel.
	lbRules []Rule
	// swRules holds session-window rules that need count refund on cancel.
	swRules []Rule
	// spendRules holds MeterSpend rules, charged at Commit.
	spendRules []Rule
	// noCommit marks a reservation that metered nothing — a check-only
	// Reserve. Committing it would be a second script call for no state.
	noCommit bool
}

// SetNoCommit marks res as metering nothing, so Commit is a no-op. Set by
// callers that reserve only to run a check (token revocation) alongside no
// rate-limit rule.
func (r *Reservation) SetNoCommit() {
	if r != nil {
		r.noCommit = true
	}
}

// needsCommit reports whether committing r with obs changes any state. An uncancelled commit of a reservation holding no concurrency slot and no token meter would only write its idempotency guard; skipping it is safe because no caller commits a reservation cancelled after committing it uncancelled.
func (r *Reservation) needsCommit(obs Observations) bool {
	if r == nil || r.noCommit {
		return false
	}
	return obs.Cancelled || len(r.conKeys) > 0 || len(r.tokRules) > 0 || (len(r.spendRules) > 0 && obs.CostNanos > 0)
}

// Reserve checks all rules and increments counters atomically via one RunScript call.
// On violation, all increments from this call are rolled back and *ExceededError is returned.
// scope is the Redis Cluster hash-tag anchor (e.g. "policy:prod-policy").
func (l *Limiter) Reserve(ctx context.Context, scope string, rules []Rule) (*Reservation, error) {
	return l.reserve(ctx, l.runner, scope, rules)
}

func (l *Limiter) reserve(ctx context.Context, runner kv.Scripter, scope string, rules []Rule) (*Reservation, error) {
	token := reqid.Generate()
	if onlyAlerts(rules) {
		for _, r := range rules {
			if _, _, err := periodBounds(r.Period, time.Time{}); err != nil {
				return nil, err
			}
		}
		return newReservation(scope, rules, token), nil
	}
	call, err := l.buildReserveCall(scope, rules, token)
	if err != nil {
		return nil, err
	}
	raw, err := runner.RunScript(ctx, call.Name, call.Script, call.Keys, call.Args...)
	if err != nil {
		l.releaseUnanswered(scope, rules, token, err)
		return nil, reserveFailed(err)
	}
	return l.decodeReserve(ctx, scope, rules, token, raw)
}

func (l *Limiter) buildReserveCall(scope string, rules []Rule, token string) (kv.ScriptCall, error) {
	now := l.clock()
	keys, ruleArgs, err := buildReserveArgs(scope, rules, now)
	if err != nil {
		return kv.ScriptCall{}, err
	}
	// A pointer spares Marshal the addressable copy it makes of a slice value; the output is the same.
	rulesJSON, err := json.Marshal(&ruleArgs)
	if err != nil {
		return kv.ScriptCall{}, fmt.Errorf("limit: marshal rules: %w", err)
	}
	// A reserve that takes a lease checks the token's commit guard, which a release for a lost reply writes, so it cannot take the lease after that release.
	releaseIdx := 0
	if holdsLease(rules) {
		keys = append(keys, commitGuardKey(scope, token))
		releaseIdx = len(keys)
	}
	return kv.ScriptCall{
		Name:   "limit.reserve",
		Script: reserveLuaScript,
		Keys:   keys,
		Args:   []any{now.UnixMilli(), rulesJSON, token, releaseIdx},
	}, nil
}

func holdsLease(rules []Rule) bool {
	for _, r := range rules {
		if r.Meter == "concurrency" {
			return true
		}
	}
	return false
}

func (l *Limiter) decodeReserve(ctx context.Context, scope string, rules []Rule, token string, raw []byte) (*Reservation, error) {
	var res reserveResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("limit: decode reserve result: %w", err)
	}

	if res.Released {
		return nil, fmt.Errorf("limit: reserve %s ran after its release", token)
	}

	if res.Revoked {
		l.log.Debug("limit reserve revoked", "request_id", reqid.From(ctx), "rule", res.RuleName)
		return nil, ErrRevoked
	}

	if res.Exceeded {
		exceeded := &ExceededError{
			RetryAfter: time.Duration(res.RetryAfterMs) * time.Millisecond,
		}
		exceeded.Rule = findRule(rules, res.RuleKey, res.RuleName, res.Meter)
		l.log.Debug("limit reserve exceeded",
			"request_id", reqid.From(ctx),
			"rule", res.RuleName,
			"meter", res.Meter,
			"retry_after_seconds", exceeded.RetryAfter.Seconds(),
		)
		return nil, exceeded
	}

	reservation := newReservation(scope, rules, token)
	l.log.Debug("limit reserve", "request_id", reqid.From(ctx), "rules", len(rules), "decision", "granted")
	return reservation, nil
}

// newReservation returns the reservation a granted reserve of rules holds, with the per-kind lists Commit reads.
func newReservation(scope string, rules []Rule, token string) *Reservation {
	reservation := &Reservation{
		ID:    token,
		scope: scope,
		rules: rules,
	}
	for _, rule := range rules {
		strategy := rule.Strategy
		if strategy == "" {
			strategy = StrategyTokenBucket
		}
		switch {
		case rule.Meter == MeterRevoked:
			// Nothing to commit or refund: the rule wrote no state.
		case rule.Meter == MeterSpend:
			reservation.spendRules = append(reservation.spendRules, rule)
		case rule.Meter == "concurrency":
			reservation.conKeys = append(reservation.conKeys, concurrencyKey(scope, rule))
		case rule.Meter == "tokens" || strings.HasPrefix(rule.Meter, "tokens."):
			reservation.tokRules = append(reservation.tokRules, rule)
		case strategy == StrategyTokenBucket:
			reservation.tbRules = append(reservation.tbRules, rule)
		case strategy == StrategyLeakyBucket:
			reservation.lbRules = append(reservation.lbRules, rule)
		case strategy == StrategySessionWindow:
			reservation.swRules = append(reservation.swRules, rule)
		}
	}
	return reservation
}

// Commit finalizes a Reservation via one RunScript call. Tokens are incremented post-hoc; concurrency leases are always released. A repeated Commit is a no-op only when the first one wrote the idempotency guard: an uncancelled commit of a reservation with no concurrency or token-meter rule settles nothing, makes no call and writes no guard, so a later cancelled Commit still refunds it.
//
// obs.Tokens is a map[string]int64. Per-meter increments are derived as:
//   - meter "tokens":        obs.Tokens["input"] + obs.Tokens["output"]
//   - meter "tokens.<key>":  obs.Tokens["<key>"]
//   - meter "requests":      always 1 (counted at Reserve; not post-hoc)
//   - meter "concurrency":   the reservation's lease released
func (l *Limiter) Commit(ctx context.Context, res *Reservation, obs Observations) error {
	if !res.needsCommit(obs) {
		return nil
	}
	call, err := l.buildCommitCall(res, obs, l.clock())
	if err != nil {
		return err
	}

	raw, err := l.runner.RunScript(ctx, call.Name, call.Script, call.Keys, call.Args...)
	if err != nil {
		metrics.KVError("limit_commit", metrics.KVDropped)
		return fmt.Errorf("limit: commit script: %w", err)
	}
	l.logCommit(res, obs, raw)
	return nil
}

// CommitBoth finalizes two reservations. When both are present and the runner
// supports batching, it commits them in a single round trip (one Redis
// pipeline) — the reservations live under different hash tags (inbound policy
// scope vs. upstream hostkey scope) so a single CROSSSLOT script is impossible;
// batching the two independent, individually-atomic commit scripts is the
// Cluster-safe way to collapse the two sequential round trips into one. Both
// reservations receive the same obs, matching the sequential Commit call pair.
//
// Falls back to sequential Commit when only one reservation exists, when the
// runner is not batch-capable, or (returning joined errors) when a batched
// call fails. Semantics are byte-identical to two Commit calls because the
// per-reservation script and args are produced by the same builder.
func (l *Limiter) CommitBoth(ctx context.Context, a, b *Reservation, obs Observations) error {
	present := make([]*Reservation, 0, 2)
	if a.needsCommit(obs) {
		present = append(present, a)
	}
	if b.needsCommit(obs) {
		present = append(present, b)
	}
	switch len(present) {
	case 0:
		return nil
	case 1:
		return l.Commit(ctx, present[0], obs)
	}

	batcher, ok := l.runner.(kv.BatchScripter)
	if !ok {
		return errors.Join(l.Commit(ctx, present[0], obs), l.Commit(ctx, present[1], obs))
	}

	now := l.clock()
	calls := make([]kv.ScriptCall, len(present))
	for i, res := range present {
		call, err := l.buildCommitCall(res, obs, now)
		if err != nil {
			return err
		}
		calls[i] = call
	}

	results := batcher.RunScriptBatch(ctx, calls)
	var errs []error
	for i, r := range results {
		if r.Err != nil {
			metrics.KVError("limit_commit", metrics.KVDropped)
			errs = append(errs, fmt.Errorf("limit: commit script: %w", r.Err))
			continue
		}
		l.logCommit(present[i], obs, r.Value)
	}
	return errors.Join(errs...)
}

// buildCommitCall produces the limit.commit script invocation for one
// reservation. It is the single source of truth for Commit and CommitBoth, so
// the batched path is guaranteed identical to the sequential one.
func (l *Limiter) buildCommitCall(res *Reservation, obs Observations, now time.Time) (kv.ScriptCall, error) {
	guardKey := commitGuardKey(res.scope, res.ID)
	guardTTLMs := commitGuardTTL.Milliseconds()

	// Build per-token-rule amounts, one entry per tokRule.
	tokAmounts := make([]int64, len(res.tokRules))
	if !obs.Cancelled {
		for i, rule := range res.tokRules {
			m := rule.Meter
			if m == "tokens" {
				// Input + output is the provider's total; cache, reasoning
				// and server-tool counts have their own tokens.<key> meters,
				// so summing the whole map would count a request several times.
				tokAmounts[i] = obs.Tokens["input"] + obs.Tokens["output"]
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

	// The four JSON arrays share one buffer, sized for the widest int64, and go to the store as sub-slices.
	const maxInt = len("-9223372036854775808")
	nRefund := len(res.tbRules) + len(res.lbRules) + len(res.swRules)
	buf := make([]byte, 0, 8+len(tokAmounts)*(maxInt+1)+nRefund*(3*maxInt+5))

	// tok_amounts: [amount, ...], one per tokRule.
	buf = append(buf, '[')
	for i, n := range tokAmounts {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = strconv.AppendInt(buf, n, 10)
	}
	buf = append(buf, ']')
	tokEnd := len(buf)

	// Refund descriptors carry 1-based KEYS indices: tb [key_idx, cost_scaled, burst], lb and sw [key_idx, cost_scaled].
	buf = append(buf, '[')
	for i, rule := range res.tbRules {
		keys = append(keys, tbStateKey(res.scope, rule))
		buf = appendRefund(buf, i, int64(len(keys)), 1000)
		buf = append(buf, ',')
		buf = strconv.AppendInt(buf, rule.Amount, 10)
		buf = append(buf, ']')
	}
	buf = append(buf, ']')
	tbEnd := len(buf)
	buf = append(buf, '[')
	for i, rule := range res.lbRules {
		keys = append(keys, lbStateKey(res.scope, rule))
		buf = append(appendRefund(buf, i, int64(len(keys)), 1000), ']')
	}
	buf = append(buf, ']')
	lbEnd := len(buf)
	buf = append(buf, '[')
	for i, rule := range res.swRules {
		keys = append(keys, swStateKey(res.scope, rule))
		buf = append(appendRefund(buf, i, int64(len(keys)), 1), ']')
	}
	buf = append(buf, ']')

	cancelledInt := int64(0)
	if obs.Cancelled {
		cancelledInt = 1
	}

	args := []any{
		res.ID,
		guardTTLMs,
		int64(len(res.conKeys)),
		int64(len(res.tokRules)),
		buf[:tokEnd:tokEnd],
		tokTTLMs,
		cancelledInt,
		buf[tokEnd:tbEnd:tbEnd],
		buf[tbEnd:lbEnd:lbEnd],
		buf[lbEnd:],
	}
	// Spend arguments ride only on reservations that hold spend rules, so every other commit sends what it always has.
	if len(res.spendRules) > 0 {
		var spends []byte
		var err error
		if spends, keys, err = appendSpends(make([]byte, 0, len(res.spendRules)*(3*maxInt+5)+2), keys, res, now); err != nil {
			return kv.ScriptCall{}, err
		}
		cost := obs.CostNanos
		if obs.Cancelled {
			cost = 0
		}
		args = append(args, spends, cost)
	}

	return kv.ScriptCall{
		Name:   "limit.commit",
		Script: commitLuaScript,
		Keys:   keys,
		Args:   args,
	}, nil
}

// CommitAll commits several reservations, which may live under different hash tags, with the same obs: one round trip when more than one settles something and the store batches scripts. It returns the spend rules whose cap this commit's charge reached, which happens once per rule and period.
func (l *Limiter) CommitAll(ctx context.Context, obs Observations, res ...*Reservation) ([]Rule, error) {
	now := l.clock()
	var (
		present []*Reservation
		calls   []kv.ScriptCall
	)
	for _, r := range res {
		if !r.needsCommit(obs) {
			continue
		}
		call, err := l.buildCommitCall(r, obs, now)
		if err != nil {
			return nil, err
		}
		present = append(present, r)
		calls = append(calls, call)
	}
	if len(calls) == 0 {
		return nil, nil
	}

	var results []kv.ScriptResult
	if batcher, ok := l.runner.(kv.BatchScripter); ok && len(calls) > 1 {
		results = batcher.RunScriptBatch(ctx, calls)
	} else {
		results = make([]kv.ScriptResult, len(calls))
		for i, c := range calls {
			results[i].Value, results[i].Err = l.runner.RunScript(ctx, c.Name, c.Script, c.Keys, c.Args...)
		}
	}
	var (
		crossed []Rule
		errs    []error
	)
	for i, r := range results {
		if r.Err != nil {
			metrics.KVError("limit_commit", metrics.KVDropped)
			errs = append(errs, fmt.Errorf("limit: commit script: %w", r.Err))
			continue
		}
		l.logCommit(present[i], obs, r.Value)
		crossed = append(crossed, crossedSpends(r.Value, present[i])...)
	}
	return crossed, errors.Join(errs...)
}

// appendRefund appends the i-th refund descriptor's opening and first two fields, "[key_idx,cost", to buf.
func appendRefund(buf []byte, i int, keyIdx, cost int64) []byte {
	if i > 0 {
		buf = append(buf, ',')
	}
	buf = append(buf, '[')
	buf = strconv.AppendInt(buf, keyIdx, 10)
	buf = append(buf, ',')
	return strconv.AppendInt(buf, cost, 10)
}

// logCommit emits the post-commit debug line, treating "noop" (duplicate
// guard) as a no-op success — identical to the prior inline handling.
func (l *Limiter) logCommit(res *Reservation, obs Observations, raw []byte) {
	if string(raw) == "noop" {
		l.log.Debug("limit commit duplicate", "reservation_id", res.ID)
		return
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
