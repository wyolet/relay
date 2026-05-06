// Package keypool implements per-key circuit-breaker state and round-robin
// Pool selection over healthy keys. State is persisted in pkg/state under
// "secret_health:<keyHash>" (circuit records) and "pool_rr:<poolName>"
// (round-robin counters).
package keypool

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"time"

	"github.com/wyolet/relay/pkg/configstore"
	"github.com/wyolet/relay/pkg/limit"
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
var ErrPoolOutOfCapacity = errors.New("keypool: pool out of capacity (all secrets at zero remaining quota)")

// backoffSchedule is seconds per step, capped at 60.
var backoffSchedule = [7]int{1, 2, 4, 8, 16, 32, 60}

const (
	stateKeyPrefix = "secret_health:"
	rrKeyPrefix    = "pool_rr:"

	// ttlFlat is the TTL applied to all non-indefinite records so they
	// persist past OpenUntil for debugging.
	ttlFlat = 24 * time.Hour
)

// Selector picks Secrets from Pools and tracks per-key circuit-breaker state.
type Selector struct {
	state   kv.Store
	log     *slog.Logger
	clock   func() time.Time
	limiter *limit.Limiter
	cfg     configstore.ConfigStore
	rng     *rand.Rand
}

// New constructs a Selector. clock, limiter, cfg, and rng may be nil.
// When limiter and cfg are nil, Pick falls back to round-robin (M2 behavior).
// When rng is nil, a new rand seeded from time.Now().UnixNano() is used.
func New(s kv.Store, log *slog.Logger, clock func() time.Time, limiter *limit.Limiter, cfg configstore.ConfigStore, rng *rand.Rand) *Selector {
	if clock == nil {
		clock = time.Now
	}
	if rng == nil {
		rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return &Selector{state: s, log: log, clock: clock, limiter: limiter, cfg: cfg, rng: rng}
}

func (s *Selector) stateKey(keyHash string) string { return stateKeyPrefix + keyHash }
func (s *Selector) rrKey(poolName string) string   { return rrKeyPrefix + poolName }

func (s *Selector) readRecord(ctx context.Context, keyHash string) circuitRecord {
	b, err := s.state.Get(ctx, s.stateKey(keyHash))
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
	if err := s.state.Set(ctx, s.stateKey(keyHash), b, ttl); err != nil {
		s.log.Error("keypool: write record failed", "key_hash", keyHash, "err", err)
	}
}

// Pick returns a healthy Secret from the pool. When a limiter and config store
// are configured, it uses quota-aware weighted-random selection; otherwise it
// falls back to round-robin (M2 behavior).
//
// Open keys past their OpenUntil are auto-transitioned to HalfOpen and become
// eligible. Concurrent Picks may both pick the same half-open key; the
// caller's RecordSuccess/RecordFailure resolves the outcome (acceptable in M2).
func (s *Selector) Pick(ctx context.Context, provider *configstore.Provider, pool *configstore.Pool, model *configstore.Model, secrets []*configstore.Secret) (*configstore.Secret, error) {
	now := s.clock()

	type candidate struct {
		secret  *configstore.Secret
		rec     circuitRecord
		promote bool
	}

	var healthy []candidate
	for _, sec := range secrets {
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

	// Weighted-random when limiter and cfg are available.
	if s.limiter != nil && s.cfg != nil {
		weights := make([]int64, len(healthy))
		anyRules := false
		for i, c := range healthy {
			rules := s.cfg.RateLimitsForRequest(provider, pool, model, c.secret)
			if len(rules) == 0 {
				weights[i] = -1 // sentinel: no rules → unbounded
				continue
			}
			anyRules = true
			remaining, err := s.limiter.RemainingByMeter(ctx, rules)
			if err != nil {
				weights[i] = -1
				continue
			}
			w := int64(-1)
			if v, ok := remaining[configstore.MeterRequests]; ok {
				if w < 0 || v < w {
					w = v
				}
			}
			if v, ok := remaining[configstore.MeterTokens]; ok {
				if w < 0 || v < w {
					w = v
				}
			}
			if w < 0 {
				// Only concurrency or unknown meters → treat as unbounded.
				weights[i] = -1
			} else {
				weights[i] = w
			}
		}

		if anyRules {
			// Replace unbounded sentinels with a high weight relative to bounded ones.
			const highWeight = int64(1<<32 - 1)
			var total int64
			for i := range weights {
				if weights[i] < 0 {
					weights[i] = highWeight
				}
				total += weights[i]
			}
			if total == 0 {
				return nil, ErrPoolOutOfCapacity
			}
			r := s.rng.Int63n(total)
			var acc int64
			for i, c := range healthy {
				acc += weights[i]
				if r < acc {
					return c.secret, nil
				}
			}
			return healthy[len(healthy)-1].secret, nil
		}
		// No secret has any rules → fall through to round-robin.
	}

	// Round-robin fallback.
	var idx int64
	err := s.state.WithLock(ctx, []string{s.rrKey(pool.Metadata.Name)}, func(ctx context.Context) error {
		var ierr error
		idx, ierr = s.state.Incr(ctx, s.rrKey(pool.Metadata.Name), 1)
		return ierr
	})
	if err != nil {
		idx = 1
	}

	chosen := healthy[(idx-1)%int64(len(healthy))]
	return chosen.secret, nil
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
