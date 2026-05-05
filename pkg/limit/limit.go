// Package limit implements a sliding-window-counter rate limiter with a
// two-phase Reserve/Commit API. Three meters are supported: requests,
// tokens, concurrency. State is persisted via pkg/state with WithLock for
// atomicity. Idempotent Commit is guaranteed via a guard key.
package limit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/wyolet/relay/pkg/configstore"
	"github.com/wyolet/relay/pkg/reqid"
	"github.com/wyolet/relay/pkg/state"
)

const (
	commitGuardTTL = 5 * time.Minute
)

// Limiter enforces rate-limit rules using pkg/state for counters.
// One Limiter is shared across the process; concurrent calls are safe.
type Limiter struct {
	state state.Store
	log   *slog.Logger
	clock func() time.Time
}

func New(s state.Store, log *slog.Logger, clock func() time.Time) *Limiter {
	if clock == nil {
		clock = time.Now
	}
	return &Limiter{state: s, log: log, clock: clock}
}

// Reservation is returned by a successful Reserve call.
type Reservation struct {
	ID          string
	rules       []configstore.ResolvedRule
	incremented []configstore.ResolvedRule // subset whose counters were incremented
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

// Reserve checks all rules and increments counters. On violation, all
// increments from this call are rolled back and *ExceededError is returned.
func (l *Limiter) Reserve(ctx context.Context, rules []configstore.ResolvedRule) (*Reservation, error) {
	now := l.clock()

	// Collect all state keys so we can lock them atomically.
	keys := collectKeys(rules, now)

	res := &Reservation{
		ID:    reqid.Generate(),
		rules: rules,
	}

	var exceeded *ExceededError

	err := l.state.WithLock(ctx, keys, func(ctx context.Context) error {
		for _, rule := range rules {
			w := rule.RateLimit.Spec.Window
			amount := rule.RateLimit.Spec.Amount
			cur, prev := windowBuckets(now, w)
			frac := fractionElapsed(now, cur, w)

			switch rule.Meter {
			case configstore.MeterRequests:
				curKey := bucketKey(rule, cur)
				prevKey := bucketKey(rule, prev)

				// Increment first, then check.
				newCur, err := l.state.Incr(ctx, curKey, 1)
				if err != nil {
					return err
				}
				_ = l.state.Expire(ctx, curKey, 2*w)
				prevVal, err := readCounter(ctx, l.state, prevKey)
				if err != nil {
					return err
				}
				rate := interpolatedRate(newCur, prevVal, frac)
				if rate > float64(amount) {
					// Roll back all increments including this one.
					if _, rerr := l.state.Incr(ctx, curKey, -1); rerr != nil {
						l.log.Error("limit: rollback failed", "key", curKey, "err", rerr)
					}
					l.rollback(ctx, res.incremented, now)
					res.incremented = nil
					ra := retryAfterRequests(newCur-1, prevVal, amount, now, cur, w)
					exceeded = &ExceededError{Rule: rule, RetryAfter: ra}
					return nil
				}
				res.incremented = append(res.incremented, rule)

			case configstore.MeterConcurrency:
				cKey := concurrencyKey(rule)
				newVal, err := l.state.Incr(ctx, cKey, 1)
				if err != nil {
					return err
				}
				_ = l.state.Expire(ctx, cKey, 5*w)
				if newVal > amount {
					if _, rerr := l.state.Incr(ctx, cKey, -1); rerr != nil {
						l.log.Error("limit: rollback failed", "key", cKey, "err", rerr)
					}
					l.rollback(ctx, res.incremented, now)
					res.incremented = nil
					exceeded = &ExceededError{Rule: rule, RetryAfter: w}
					return nil
				}
				res.incremented = append(res.incremented, rule)

			case configstore.MeterTokens:
				// Peek only — no increment at Reserve time.
				curKey := bucketKey(rule, cur)
				prevKey := bucketKey(rule, prev)
				curVal, err := readCounter(ctx, l.state, curKey)
				if err != nil {
					return err
				}
				prevVal, err := readCounter(ctx, l.state, prevKey)
				if err != nil {
					return err
				}
				rate := interpolatedRate(curVal, prevVal, frac)
				if rate >= float64(amount) {
					l.rollback(ctx, res.incremented, now)
					res.incremented = nil
					ra := retryAfterRequests(curVal, prevVal, amount, now, cur, w)
					exceeded = &ExceededError{Rule: rule, RetryAfter: ra}
					return nil
				}
				// No increment; not added to incremented list.
			}
		}
		return nil
	})

	if err != nil {
		return nil, err
	}

	if exceeded != nil {
		l.log.Info("limit reserve exceeded",
			"request_id", reqid.From(ctx),
			"rule", fmt.Sprintf("%s:%s:%s", exceeded.Rule.ParentName, exceeded.Rule.RateLimit.Metadata.Name, exceeded.Rule.Meter),
			"retry_after_seconds", exceeded.RetryAfter.Seconds(),
		)
		return nil, exceeded
	}

	l.log.Debug("limit reserve",
		"request_id", reqid.From(ctx),
		"rules", len(rules),
		"decision", "granted",
	)
	return res, nil
}

// rollback decrements all incremented counters. Must be called under lock.
func (l *Limiter) rollback(ctx context.Context, incremented []configstore.ResolvedRule, now time.Time) {
	for _, rule := range incremented {
		w := rule.RateLimit.Spec.Window
		switch rule.Meter {
		case configstore.MeterRequests:
			cur, _ := windowBuckets(now, w)
			if _, err := l.state.Incr(ctx, bucketKey(rule, cur), -1); err != nil {
				l.log.Error("limit: rollback requests failed", "err", err)
			}
		case configstore.MeterConcurrency:
			if _, err := l.state.Incr(ctx, concurrencyKey(rule), -1); err != nil {
				l.log.Error("limit: rollback concurrency failed", "err", err)
			}
		}
	}
}

// Commit finalizes a Reservation. Tokens are incremented post-hoc; concurrency
// is always decremented. Calling Commit twice is a no-op.
func (l *Limiter) Commit(ctx context.Context, res *Reservation, obs Observations) error {
	guard := commitGuardKey(res.ID)

	// Idempotency check.
	if _, err := l.state.Get(ctx, guard); err == nil {
		l.log.Debug("limit commit duplicate", "reservation_id", res.ID)
		return nil
	}

	l.log.Debug("limit commit",
		"reservation_id", res.ID,
		"tokens", obs.Tokens,
		"cancelled", obs.Cancelled,
	)

	now := l.clock()

	for _, rule := range res.incremented {
		switch rule.Meter {
		case configstore.MeterConcurrency:
			cKey := concurrencyKey(rule)
			if _, err := l.state.Incr(ctx, cKey, -1); err != nil {
				l.log.Error("limit commit: decrement concurrency failed", "err", err)
			}
		case configstore.MeterRequests:
			// Already incremented at Reserve; no action needed.
		}
	}

	// Token meters: retroactively add tokens (not tracked in incremented since no Reserve-time increment).
	if !obs.Cancelled && obs.Tokens > 0 {
		for _, rule := range res.rules {
			if rule.Meter != configstore.MeterTokens {
				continue
			}
			w := rule.RateLimit.Spec.Window
			cur, _ := windowBuckets(now, w)
			tKey := bucketKey(rule, cur)
			newVal, err := l.state.Incr(ctx, tKey, obs.Tokens)
			if err != nil {
				l.log.Error("limit commit: increment tokens failed", "err", err)
				continue
			}
			_ = l.state.Expire(ctx, tKey, 2*w)
			_ = newVal
		}
	}

	// Set guard key.
	_ = l.state.Set(ctx, guard, []byte("1"), commitGuardTTL)
	return nil
}

// RemainingByMeter returns the smallest remaining capacity per meter across rules.
func (l *Limiter) RemainingByMeter(ctx context.Context, rules []configstore.ResolvedRule) (map[configstore.Meter]int64, error) {
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
			curKey := bucketKey(rule, cur)
			prevKey := bucketKey(rule, prev)
			curVal, err := readCounter(ctx, l.state, curKey)
			if err != nil {
				return nil, err
			}
			prevVal, err := readCounter(ctx, l.state, prevKey)
			if err != nil {
				return nil, err
			}
			rate = interpolatedRate(curVal, prevVal, frac)

		case configstore.MeterConcurrency:
			cVal, err := readCounter(ctx, l.state, concurrencyKey(rule))
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

// collectKeys gathers all state keys touched for locking.
func collectKeys(rules []configstore.ResolvedRule, now time.Time) []string {
	seen := make(map[string]struct{})
	for _, rule := range rules {
		w := rule.RateLimit.Spec.Window
		cur, prev := windowBuckets(now, w)
		switch rule.Meter {
		case configstore.MeterRequests, configstore.MeterTokens:
			seen[bucketKey(rule, cur)] = struct{}{}
			seen[bucketKey(rule, prev)] = struct{}{}
		case configstore.MeterConcurrency:
			seen[concurrencyKey(rule)] = struct{}{}
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
