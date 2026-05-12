// Package keypool implements per-key circuit-breaker state and configurable
// Pool selection over healthy keys. State is persisted in pkg/state under
// "secret_health:<keyHash>" (circuit records), "pool_rr:<poolName>"
// (round-robin counters), and "pool_lru:<poolName>:<keyHash>"
// (LRU timestamps).
//
// Supported selection strategies (catalog.KeySelection):
//   - "prioritized" (default) — always pick the first healthy key in declaration order.
//   - "round-robin" — distribute traffic evenly using a counter.
//   - "least-recently-used" — pick the key with the oldest last-used timestamp.
package keypool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strconv"
	"time"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/pkg/reqid"
	"github.com/wyolet/relay/pkg/kv"
)

// FailureKind classifies the upstream failure for circuit-breaker transitions.
type FailureKind int

const (
	FailureAuth            FailureKind = iota // 401/403 → open indefinitely
	FailureRateLimitShort                     // 429 with Retry-After ≤ 5s → stay closed
	FailureRateLimitLong                      // 429 with Retry-After > 5s → open for that duration
	FailureServerError                        // 5xx → exponential backoff
	FailureNetwork                            // net/timeout → treat as 5xx
)

// CircuitState describes the current health of a key.
type CircuitState int

const (
	CircuitClosed   CircuitState = iota // healthy, accepting traffic
	CircuitOpen                         // unhealthy, skip
	CircuitHalfOpen                     // single probe allowed
)

var ErrNoHealthyKeys = errors.New("keypool: no healthy keys in pool")

// candidate holds a healthy Secret alongside its circuit record.
type candidate struct {
	secret  *catalog.Secret
	rec     circuitRecord
	promote bool
}

// backoffSchedule is seconds per step, capped at 60.
var backoffSchedule = [7]int{1, 2, 4, 8, 16, 32, 60}

const (
	// ttlFlat is the TTL applied to all non-indefinite circuit-breaker records
	// so they persist past OpenUntil for debugging.
	ttlFlat = 24 * time.Hour

	// ttlRoundRobin is the TTL on pool_rr counters. The counter is a modular
	// index so staleness is harmless, but a long TTL lets Redis reclaim keys
	// from deleted pools instead of accumulating indefinitely.
	ttlRoundRobin = 30 * 24 * time.Hour

	// ttlLRU is the TTL on pool_lru timestamps. Long enough to survive between
	// pool deployments; staleness is harmless (treated as never-used).
	ttlLRU = 30 * 24 * time.Hour
)

// Selector picks Secrets from Pools and tracks per-key circuit-breaker state.
type Selector struct {
	state kv.Store
	log   *slog.Logger
	clock func() time.Time
	rng   *rand.Rand
}

// New constructs a Selector. clock and rng may be nil.
// When rng is nil, a new rand seeded from time.Now().UnixNano() is used.
func New(s kv.Store, log *slog.Logger, clock func() time.Time, rng *rand.Rand) *Selector {
	if clock == nil {
		clock = time.Now
	}
	if rng == nil {
		rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return &Selector{state: s, log: log, clock: clock, rng: rng}
}

func (s *Selector) readRecord(ctx context.Context, keyHash string) circuitRecord {
	b, err := s.state.Get(ctx, circuitKey(keyHash))
	if err != nil || len(b) == 0 {
		return circuitRecord{State: CircuitClosed}
	}
	r, err := decodeRecord(b)
	if err != nil {
		return circuitRecord{State: CircuitClosed}
	}
	return r
}

func (s *Selector) writeRecord(ctx context.Context, keyHash string, r circuitRecord) {
	b, err := encodeRecord(r)
	if err != nil {
		s.log.Error("keypool: encode record failed", "key_hash", keyHash, "err", err)
		return
	}
	ttl := ttlFlat
	if r.Indefinite {
		ttl = 0 // no expiry
	}
	if err := s.state.Set(ctx, circuitKey(keyHash), b, ttl); err != nil {
		s.log.Error("keypool: write record failed", "key_hash", keyHash, "err", err)
	}
}

// lruKey returns the Redis key used to store the last-use timestamp for a
// secret within a named pool.
func lruKey(poolName, keyHash string) string {
	return fmt.Sprintf("{pool_lru:%s}:%s", poolName, keyHash)
}

// Pick returns a healthy Secret from pool. The selection strategy is
// determined by pool.Spec.KeySelection (default: round-robin).
//
// exclude is an optional list of secrets to skip even if healthy (e.g. for
// retry-with-exclusion in future callers). Pass nil to skip the check.
//
// Open keys past their OpenUntil are auto-transitioned to HalfOpen and become
// eligible. Concurrent Picks may both pick the same half-open key; the
// caller's RecordSuccess/RecordFailure resolves the outcome (acceptable).
func (s *Selector) Pick(ctx context.Context, provider *catalog.Provider, pool *catalog.Policy, model *catalog.Model, secrets []*catalog.Secret, exclude ...[]*catalog.Secret) (*catalog.Secret, error) {
	now := s.clock()

	// Build exclude set for O(1) lookup.
	var excludeSet map[string]struct{}
	if len(exclude) > 0 && len(exclude[0]) > 0 {
		excludeSet = make(map[string]struct{}, len(exclude[0]))
		for _, sec := range exclude[0] {
			excludeSet[sec.KeyHash] = struct{}{}
		}
	}

	var healthy []candidate
	for _, sec := range secrets {
		if excludeSet != nil {
			if _, skip := excludeSet[sec.KeyHash]; skip {
				continue
			}
		}
		rec := s.readRecord(ctx, sec.KeyHash)

		switch rec.State {
		case CircuitOpen:
			if rec.Indefinite {
				continue
			}
			if now.Before(rec.OpenUntil) {
				continue
			}
			prior := rec.State
			rec.State = CircuitHalfOpen
			rec.LastTransition = now
			s.writeRecord(ctx, sec.KeyHash, rec)
			s.log.Info("keypool transition",
				"request_id", reqid.From(ctx),
				"key_hash", sec.KeyHash,
				"from_state", stateName(prior),
				"to_state", stateName(rec.State),
				"reason", "open_expired",
				"backoff_step", rec.BackoffStep,
				"open_for_seconds", 0,
			)
			healthy = append(healthy, candidate{secret: sec, rec: rec, promote: true})
		case CircuitHalfOpen:
			healthy = append(healthy, candidate{secret: sec, rec: rec})
		case CircuitClosed:
			healthy = append(healthy, candidate{secret: sec, rec: rec})
		}
	}

	if len(healthy) == 0 {
		return nil, ErrNoHealthyKeys
	}

	strategy := catalog.KeySelectionPrioritized
	if pool != nil && pool.Spec.KeySelection != "" {
		strategy = pool.Spec.KeySelection
	}

	switch strategy {
	case catalog.KeySelectionRoundRobin:
		return s.pickRoundRobin(ctx, pool, healthy)

	case catalog.KeySelectionLeastRecentlyUsed:
		return s.pickLRU(ctx, pool, healthy)

	default: // "prioritized" or empty — first healthy in declaration order.
		return healthy[0].secret, nil
	}
}

// pickRoundRobin selects a candidate using a modular counter stored in Redis.
func (s *Selector) pickRoundRobin(ctx context.Context, pool *catalog.Policy, healthy []candidate) (*catalog.Secret, error) {
	var idx int64
	err := s.state.WithLock(ctx, []string{roundRobinKey(pool.Metadata.Name)}, func(ctx context.Context) error {
		var ierr error
		idx, ierr = s.state.Incr(ctx, roundRobinKey(pool.Metadata.Name), 1)
		if ierr == nil {
			// Refresh 30-day TTL on every increment. Redis reclaims counters for
			// deleted pools without affecting modular-index correctness.
			_ = s.state.Expire(ctx, roundRobinKey(pool.Metadata.Name), ttlRoundRobin)
		}
		return ierr
	})
	if err != nil {
		idx = 1
	}
	chosen := healthy[(idx-1)%int64(len(healthy))]
	return chosen.secret, nil
}

// pickLRU selects the healthy candidate with the oldest last-use timestamp
// (or never-used) and updates its timestamp.
func (s *Selector) pickLRU(ctx context.Context, pool *catalog.Policy, healthy []candidate) (*catalog.Secret, error) {
	var (
		chosen    *catalog.Secret
		chosenTS  int64 = -1 // -1 = not yet set
		neverUsed *catalog.Secret
	)

	for _, c := range healthy {
		k := lruKey(pool.Metadata.Name, c.secret.KeyHash)
		raw, err := s.state.Get(ctx, k)
		if err != nil || len(raw) == 0 {
			// Never used — immediately preferred.
			if neverUsed == nil {
				neverUsed = c.secret
			}
			continue
		}
		ts, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil {
			if neverUsed == nil {
				neverUsed = c.secret
			}
			continue
		}
		if chosen == nil || ts < chosenTS {
			chosen = c.secret
			chosenTS = ts
		}
	}

	if neverUsed != nil {
		chosen = neverUsed
	}
	if chosen == nil {
		// Fallback: should not happen given len(healthy) > 0.
		chosen = healthy[0].secret
	}

	// Stamp last-use timestamp.
	now := s.clock().UnixMilli()
	k := lruKey(pool.Metadata.Name, chosen.KeyHash)
	_ = s.state.Set(ctx, k, []byte(strconv.FormatInt(now, 10)), ttlLRU)

	return chosen, nil
}

// PickWithExclude is a convenience wrapper around Pick that accepts an explicit
// exclude list. Callers that always have an exclude slice can use this to avoid
// the variadic syntax.
func (s *Selector) PickWithExclude(ctx context.Context, provider *catalog.Provider, pool *catalog.Policy, model *catalog.Model, secrets []*catalog.Secret, exclude []*catalog.Secret) (*catalog.Secret, error) {
	return s.Pick(ctx, provider, pool, model, secrets, exclude)
}

// ClearCircuit deletes the circuit-breaker record for a secret from the KV
// store. This is best-effort: if the DEL fails the error is returned to the
// caller, who should log a warning but not fail the outer operation.
//
// Use this when a secret is permanently deleted from the catalog so that
// orphaned secret_health:* keys do not accumulate in Redis indefinitely (R-8).
func ClearCircuit(ctx context.Context, store kv.Store, keyHash string) error {
	return store.Del(ctx, circuitKey(keyHash))
}

// RecordSuccess transitions a key to CircuitClosed and resets backoff.
func (s *Selector) RecordSuccess(ctx context.Context, keyHash string) {
	now := s.clock()
	prior := s.readRecord(ctx, keyHash)
	rec := circuitRecord{
		State:          CircuitClosed,
		BackoffStep:    0,
		LastTransition: now,
	}
	s.writeRecord(ctx, keyHash, rec)
	s.log.Info("keypool transition",
		"request_id", reqid.From(ctx),
		"key_hash", keyHash,
		"from_state", stateName(prior.State),
		"to_state", stateName(rec.State),
		"reason", "success",
		"backoff_step", rec.BackoffStep,
		"open_for_seconds", 0,
	)
}

// RecordFailure transitions according to kind. retryAfter is honoured only
// for RateLimit kinds.
func (s *Selector) RecordFailure(ctx context.Context, keyHash string, kind FailureKind, retryAfter time.Duration) {
	now := s.clock()
	rec := s.readRecord(ctx, keyHash)
	prior := rec.State

	switch kind {
	case FailureAuth:
		rec.State = CircuitOpen
		rec.Indefinite = true
		rec.OpenUntil = time.Time{}
		rec.LastTransition = now
		s.writeRecord(ctx, keyHash, rec)
		s.log.Info("keypool transition",
			"request_id", reqid.From(ctx),
			"key_hash", keyHash,
			"from_state", stateName(prior),
			"to_state", stateName(rec.State),
			"reason", "401",
			"backoff_step", rec.BackoffStep,
			"open_for_seconds", 0,
		)

	case FailureRateLimitShort:
		// Stay closed; no state change.
		s.log.Info("keypool transition",
			"request_id", reqid.From(ctx),
			"key_hash", keyHash,
			"from_state", stateName(prior),
			"to_state", stateName(prior),
			"reason", "rate_limit_short",
			"backoff_step", rec.BackoffStep,
			"open_for_seconds", 0,
		)
		return

	case FailureRateLimitLong:
		rec.State = CircuitOpen
		rec.Indefinite = false
		rec.OpenUntil = now.Add(retryAfter)
		rec.LastTransition = now
		s.writeRecord(ctx, keyHash, rec)
		s.log.Info("keypool transition",
			"request_id", reqid.From(ctx),
			"key_hash", keyHash,
			"from_state", stateName(prior),
			"to_state", stateName(rec.State),
			"reason", "rate_limit_long",
			"backoff_step", rec.BackoffStep,
			"open_for_seconds", int(retryAfter.Seconds()),
		)

	case FailureServerError, FailureNetwork:
		reason := "5xx"
		if kind == FailureNetwork {
			reason = "network"
		}
		step := rec.BackoffStep + 1
		if step >= len(backoffSchedule) {
			step = len(backoffSchedule) - 1
		}
		rec.BackoffStep = step
		dur := time.Duration(backoffSchedule[step]) * time.Second
		rec.State = CircuitOpen
		rec.Indefinite = false
		rec.OpenUntil = now.Add(dur)
		rec.LastTransition = now
		s.writeRecord(ctx, keyHash, rec)
		s.log.Info("keypool transition",
			"request_id", reqid.From(ctx),
			"key_hash", keyHash,
			"from_state", stateName(prior),
			"to_state", stateName(rec.State),
			"reason", reason,
			"backoff_step", rec.BackoffStep,
			"open_for_seconds", int(dur.Seconds()),
		)
	}
}
