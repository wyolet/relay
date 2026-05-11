// Package ratelimit implements a sliding-window-counter rate limiter with a
// two-phase Reserve/Commit API. Five strategies are supported: sliding-window,
// fixed-window, token-bucket, leaky-bucket, session-window. Concurrency is a
// special gauge meter that works with any strategy.
//
// This package is pure — it has no imports from github.com/wyolet/relay/internal.
// Relay-specific translation (catalog.ResolvedRule → Rule) lives in
// internal/ratelimit.
//
// Expected kv ops per request:
//   - Reserve:  1 RunScript call (all counter checks + increments atomic).
//   - Commit:   1 RunScript call (concurrency decrement + token post-increment).
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
// The caller is responsible for building Rule.Key as a stable, unique
// string that identifies the (scope, rate-limit-name, meter) tuple.
type Rule struct {
	// Key is the caller-built scope identifier used as the last segment of
	// Redis keys. Example: "Route:test-route:rl-basic".
	// The pkg prepends "limit:{<scope>}:" and a strategy prefix.
	Key string

	// Name is for human-readable error messages only (e.g. "requests on rl-basic").
	Name string

	// Meter is "requests" | "tokens" | "tokens.<suffix>" | "concurrency".
	Meter string

	// Strategy controls the rate-limit algorithm.
	Strategy Strategy

	// Amount is the budget (requests, tokens, or concurrency slots).
	Amount int64

	// Window is the measurement period. For concurrency rules the window is
	// used only to set key TTLs.
	Window time.Duration
}

// Observations are passed to Commit to supply post-hoc measurements.
// Tokens is a map of token-type → count (e.g. {"input": 300, "output": 200}).
// Legacy callers that only have a total may pass {"tokens": total}.
// Nil Tokens is treated as all-zero.
type Observations struct {
	Tokens    map[string]int64
	Cancelled bool
}

// ExceededError is returned by Reserve when a budget is violated.
type ExceededError struct {
	Rule         Rule
	RetryAfter   time.Duration
}

func (e *ExceededError) Error() string {
	return fmt.Sprintf("limit: budget exceeded: %s retry_after=%.0fs",
		e.Rule.Name, e.RetryAfter.Seconds())
}

func (e *ExceededError) Unwrap() error { return ErrExceeded }

// ErrExceeded is the sentinel wrapped by *ExceededError.
var ErrExceeded = errors.New("limit: budget exceeded")
