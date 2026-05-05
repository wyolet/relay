// Package keypool implements per-key circuit-breaker state and round-robin
// Pool selection over healthy keys. State is persisted in pkg/state under
// "secret_health:<keyHash>" (circuit records) and "pool_rr:<poolName>"
// (round-robin counters).
package keypool

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/wyolet/relay/pkg/configstore"
	"github.com/wyolet/relay/pkg/reqid"
	"github.com/wyolet/relay/pkg/state"
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
	state state.Store
	log   *slog.Logger
	clock func() time.Time
}

// New constructs a Selector. clock may be nil (defaults to time.Now).
func New(s state.Store, log *slog.Logger, clock func() time.Time) *Selector {
	if clock == nil {
		clock = time.Now
	}
	return &Selector{state: s, log: log, clock: clock}
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

// Pick returns the next healthy Secret in pool via round-robin over the
// healthy subset of secrets (input order; caller passes alphabetically sorted
// slice from configstore.SecretsForPool for deterministic distribution).
//
// Open keys past their OpenUntil are auto-transitioned to HalfOpen and become
// eligible. Concurrent Picks may both pick the same half-open key; the
// caller's RecordSuccess/RecordFailure resolves the outcome (acceptable in M2).
func (s *Selector) Pick(ctx context.Context, pool *configstore.Pool, secrets []*configstore.Secret) (*configstore.Secret, error) {
	now := s.clock()

	type candidate struct {
		secret *configstore.Secret
		rec    circuitRecord
		promote bool // was Open→HalfOpen this call
	}

	var healthy []candidate
	for _, sec := range secrets {
		rec := s.readRecord(ctx, sec.KeyHash)

		switch rec.State {
		case CircuitOpen:
			if rec.Indefinite {
				continue // never eligible
			}
			if now.Before(rec.OpenUntil) {
				continue // still open
			}
			// Expired open window → promote to half-open.
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

	// Atomic round-robin counter under lock so concurrent Picks are ordered.
	var idx int64
	err := s.state.WithLock(ctx, []string{s.rrKey(pool.Metadata.Name)}, func(ctx context.Context) error {
		var ierr error
		idx, ierr = s.state.Incr(ctx, s.rrKey(pool.Metadata.Name), 1)
		return ierr
	})
	if err != nil {
		// Fall back to first healthy key rather than failing.
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
