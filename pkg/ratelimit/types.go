// Package ratelimit implements a sliding-window-counter rate limiter with a
// two-phase Reserve/Commit API. Five strategies are supported: sliding-window,
// fixed-window, token-bucket, leaky-bucket, session-window. Concurrency is a
// special meter that works with any strategy: each reservation holds a lease
// that Commit releases and that otherwise expires after the rule's window.
//
// This package is pure — it has no imports from github.com/wyolet/relay/internal.
// Relay-specific translation (catalog.ResolvedRule → Rule) lives in
// internal/ratelimit.
//
// MeterSpend is a spend cap per calendar period rather than a rate: checked at Reserve, charged at Commit.
//
// Expected kv ops per request:
//   - Reserve:    1 RunScript call (all counter checks + increments atomic), or none when every rule is an Alert spend rule, which Reserve does not check.
//   - PrepareReserve: the same script queued on the caller's kv.Batch, so it costs no round trip of its own.
//   - A reserve that failed after it may have run, with a concurrency rule: 1 detached RunScript releasing its lease, on the store ReleaseUnansweredOn names.
//   - Commit:     1 RunScript call (concurrency lease release + token post-increment), or none when it settles nothing: an uncancelled request whose rules meter neither concurrency nor tokens. PrepareCommit queues the same script on a caller's kv.Batch.
//   - CommitBoth: 1 batched round trip committing two reservations that live
//     under different hash tags (inbound policy scope + upstream hostkey scope),
//     replacing two sequential Commit calls. Each reservation's script is
//     individually atomic; the batch is not cross-atomic (a single CROSSSLOT
//     script across the two tags is impossible on Redis Cluster).
//   - CommitAll:  the same for any number of reservations: 1 batched round trip, or 1 RunScript when one settles something.
package ratelimit

import (
	"errors"
	"fmt"
	"time"
)

// Strategy names the rate-limiting algorithm for a Rule.
type Strategy string

const (
	// StrategyTokenBucket is the default. Tokens refill continuously at
	// amount/window rate; burst = amount.
	StrategyTokenBucket Strategy = "token-bucket"

	// StrategySlidingWindow uses a two-bucket weighted interpolation.
	StrategySlidingWindow Strategy = "sliding-window"

	// StrategyFixedWindow resets the counter at every floor(now/window) boundary.
	StrategyFixedWindow Strategy = "fixed-window"

	// StrategyLeakyBucket drains at a constant rate; excess is rejected.
	StrategyLeakyBucket Strategy = "leaky-bucket"

	// StrategySessionWindow anchors the window to the first request after a
	// reset; the window does not reset in the background.
	StrategySessionWindow Strategy = "session-window"
)

// Rule is a single rate-limit rule passed to Reserve by the caller.
// The caller is responsible for building Rule.Key as a stable, unique string that identifies the rule within its scope; a counter lives as long as its Key does.
type Rule struct {
	// Key is the caller-built scope identifier used as the last segment of
	// Redis keys. Example: "Route:test-route:rl-basic".
	// The pkg prepends "limit:{<scope>}:" and a strategy prefix.
	Key string

	// Name is for human-readable error messages only (e.g. "requests on rl-basic").
	Name string

	// Meter is "requests" | "tokens" | "tokens.<suffix>" | "concurrency" |
	// MeterRevoked | MeterSpend.
	Meter string

	// Strategy controls the rate-limit algorithm.
	Strategy Strategy

	// Amount is the budget (requests, tokens, or concurrency slots).
	Amount int64

	// Window is the measurement period. For concurrency rules it is the lease
	// length: a slot whose Commit never arrives frees after it, and a request
	// running longer stops counting.
	Window time.Duration

	// Period is the calendar period a MeterSpend rule's Amount covers; such a rule ignores Window and Strategy.
	Period Period

	// Alert makes a MeterSpend rule count without refusing: Reserve skips it and Commit still charges it.
	Alert bool
}

// MeterSpend counts spend in USD nanos per calendar period in UTC. Reserve refuses once the period's spend has reached Amount (>=, unlike the other meters' >) and reserves nothing; Commit charges Observations.CostNanos, so requests in flight may overshoot by their own cost. A new period starts from zero.
const MeterSpend = "spend"

// Period is a calendar period in UTC.
type Period string

const (
	PeriodDay Period = "day"
	// PeriodWeek starts on Monday (ISO 8601).
	PeriodWeek  Period = "week"
	PeriodMonth Period = "month"
)

// MeterRevoked is a pseudo-meter: it carries no budget, only the key whose
// existence revokes the caller's credential. Callers ride it on the Reserve
// they already run so revocation costs no extra round trip.
const MeterRevoked = "revoked"

// ErrRevoked is returned by Reserve when a MeterRevoked rule's key exists.
var ErrRevoked = errors.New("limit: credential revoked")

// Observations are passed to Commit to supply post-hoc measurements.
// Tokens is a map of token-type → count (e.g. {"input": 300, "output": 200}).
// Legacy callers that only have a total may pass {"tokens": total}.
// Nil Tokens is treated as all-zero.
type Observations struct {
	Tokens    map[string]int64
	Cancelled bool
	// CostNanos is the request's priced cost in USD nanos, charged to its MeterSpend rules unless Cancelled.
	CostNanos int64
}

// KeyQuotaExhausted is returned by Reserve when a budget is violated.
// The name reflects that exceeding a per-key rule is the primary signal this
// type carries in the post-Pick path (pipeline issue #89).
type KeyQuotaExhausted struct {
	Rule       Rule
	RetryAfter time.Duration
}

func (e *KeyQuotaExhausted) Error() string {
	return fmt.Sprintf("limit: budget exceeded: %s retry_after=%.0fs",
		e.Rule.Name, e.RetryAfter.Seconds())
}

func (e *KeyQuotaExhausted) Unwrap() error { return ErrExceeded }

// ExceededError is an alias kept for backward compatibility with callers that
// use the old name. New code should use KeyQuotaExhausted directly.
//
// Deprecated: Use KeyQuotaExhausted.
type ExceededError = KeyQuotaExhausted

// ErrExceeded is the sentinel wrapped by *KeyQuotaExhausted.
var ErrExceeded = errors.New("limit: budget exceeded")
