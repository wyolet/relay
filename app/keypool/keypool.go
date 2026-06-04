// Package keypool implements per-key circuit-breaker state and configurable
// Pool selection over healthy keys. State is persisted in pkg/state under
// "secret_health:<keyHash>" (circuit records), "pool_rr:<poolName>"
// (round-robin counters), and "pool_lru:<poolName>:<keyHash>"
// (LRU timestamps).
//
// Supported selection strategies (KeySelection):
//   - "prioritized" (default) — always pick the first healthy key in declaration order.
//   - "round-robin" — distribute traffic evenly using a counter.
//   - "least-recently-used" — pick the key with the oldest last-used timestamp.
//
// The KeySelection enum lives in this package (not policy) so policy can
// import keypool without creating a cycle when its Service composes
// Selector + Limiter at runtime.
package keypool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/metrics"
	"github.com/wyolet/relay/pkg/reqid"
)

// KeySelection is the algorithm a Selector uses to pick from healthy
// candidates. Persisted as a string in Policy.Spec.KeySelection.
type KeySelection string

const (
	// KeySelectionPrioritized drains keys in declaration order — the
	// first healthy key in the candidate set wins. Default.
	KeySelectionPrioritized KeySelection = "prioritized"
	// KeySelectionRoundRobin rotates evenly across healthy keys via a
	// per-scope counter.
	KeySelectionRoundRobin KeySelection = "round-robin"
	// KeySelectionLeastRecentlyUsed prefers the key whose last successful
	// use was furthest in the past.
	KeySelectionLeastRecentlyUsed KeySelection = "least-recently-used"
)

// FailureKind classifies the upstream failure for circuit-breaker transitions.
type FailureKind int

const (
	FailureAuth           FailureKind = iota // 401/403 → open indefinitely
	FailureRateLimitShort                    // 429 with Retry-After ≤ 5s → stay closed
	FailureRateLimitLong                     // 429 with Retry-After > 5s → open for that duration
	FailureServerError                       // 5xx → exponential backoff
	FailureNetwork                           // net/timeout (post-connect) → treat as 5xx
	// FailureUpstreamUnreachable is a dial-phase failure (connection refused,
	// no route, DNS, TLS handshake): the connection was never established, so
	// it is a property of the host/baseURL, not the key. It NEVER trips a key
	// breaker — the pipeline retries the same host with backoff and reports an
	// unreachable status. Distinguishes a misconfigured baseURL from a bad key.
	FailureUpstreamUnreachable
)

// CircuitState describes the current health of a key.
type CircuitState int

const (
	CircuitClosed   CircuitState = iota // healthy, accepting traffic
	CircuitOpen                         // unhealthy, skip
	CircuitHalfOpen                     // single probe allowed
)

var ErrNoHealthyKeys = errors.New("keypool: no healthy keys in pool")

// isAnonymous reports whether keyHash belongs to the synthetic no-auth key
// routing injects for a NoAuth host. Such keys are exempt from the circuit
// breaker: there is no credential to fail over to and nothing to heal.
func isAnonymous(keyHash string) bool {
	return strings.HasPrefix(keyHash, hostkey.AnonIDPrefix)
}

// candidate holds a healthy HostKey alongside its circuit record.
type candidate struct {
	key     *hostkey.HostKey
	rec     CircuitRecord
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

// Selector picks HostKeys from Pools and tracks per-key circuit-breaker state.
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

func (s *Selector) readRecord(ctx context.Context, keyHash string) CircuitRecord {
	b, err := s.state.Get(ctx, circuitKey(keyHash))
	if err != nil || len(b) == 0 {
		return CircuitRecord{State: CircuitClosed}
	}
	r, err := decodeRecord(b)
	if err != nil {
		return CircuitRecord{State: CircuitClosed}
	}
	return r
}

// ReadCircuit returns the stored circuit-breaker record for a key and whether
// a record actually exists in the state store. A missing or undecodable record
// yields a default-closed record with found=false — i.e. the key has never
// failed and is assumed healthy. This is a read-only accessor for the admin
// plane; it does not auto-transition expired-open records the way Pick does.
func (s *Selector) ReadCircuit(ctx context.Context, keyHash string) (CircuitRecord, bool) {
	b, err := s.state.Get(ctx, circuitKey(keyHash))
	if err != nil || len(b) == 0 {
		return CircuitRecord{State: CircuitClosed}, false
	}
	r, err := decodeRecord(b)
	if err != nil {
		return CircuitRecord{State: CircuitClosed}, false
	}
	return r, true
}

func (s *Selector) writeRecord(ctx context.Context, keyHash string, r CircuitRecord) {
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
// key within a named pool.
func lruKey(poolName, keyHash string) string {
	return fmt.Sprintf("{pool_lru:%s}:%s", poolName, keyHash)
}

// Pick returns a healthy HostKey from the candidate set. scope is the
// kv key tag (typically the owning Policy's Meta.Name) used for the
// round-robin counter and LRU timestamps. algo is the selection
// strategy; empty falls back to KeySelectionPrioritized.
//
// exclude is an optional list of keys to skip even if healthy (e.g. for
// retry-with-exclusion in future callers). Pass nil to skip the check.
//
// Open keys past their OpenUntil are auto-transitioned to HalfOpen and become
// eligible. Concurrent Picks may both pick the same half-open key; the
// caller's RecordSuccess/RecordFailure resolves the outcome (acceptable).
func (s *Selector) Pick(ctx context.Context, scope string, algo KeySelection, keys []*hostkey.HostKey, exclude ...[]*hostkey.HostKey) (*hostkey.HostKey, error) {
	now := s.clock()

	// Build exclude set for O(1) lookup.
	var excludeSet map[string]struct{}
	if len(exclude) > 0 && len(exclude[0]) > 0 {
		excludeSet = make(map[string]struct{}, len(exclude[0]))
		for _, k := range exclude[0] {
			excludeSet[k.KeyHash] = struct{}{}
		}
	}

	var healthy []candidate
	for _, k := range keys {
		if excludeSet != nil {
			if _, skip := excludeSet[k.KeyHash]; skip {
				continue
			}
		}
		// The synthetic no-auth key has no credential to fail over to and
		// nothing to heal — a breaker would only strand the host's sole
		// candidate. Always treat it as healthy.
		if isAnonymous(k.KeyHash) {
			healthy = append(healthy, candidate{key: k, rec: CircuitRecord{State: CircuitClosed}})
			continue
		}
		rec := s.readRecord(ctx, k.KeyHash)

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
			s.writeRecord(ctx, k.KeyHash, rec)
			s.log.Debug("keypool transition",
				"request_id", reqid.From(ctx),
				"key_hash", k.KeyHash,
				"from_state", stateName(prior),
				"to_state", stateName(rec.State),
				"reason", "open_expired",
				"backoff_step", rec.BackoffStep,
				"open_for_seconds", 0,
			)
			healthy = append(healthy, candidate{key: k, rec: rec, promote: true})
		case CircuitHalfOpen:
			healthy = append(healthy, candidate{key: k, rec: rec})
		case CircuitClosed:
			healthy = append(healthy, candidate{key: k, rec: rec})
		}
	}

	if len(healthy) == 0 {
		return nil, ErrNoHealthyKeys
	}

	strategy := algo
	if strategy == "" {
		strategy = KeySelectionPrioritized
	}

	switch strategy {
	case KeySelectionRoundRobin:
		return s.pickRoundRobin(ctx, scope, healthy)

	case KeySelectionLeastRecentlyUsed:
		return s.pickLRU(ctx, scope, healthy)

	default: // "prioritized" or empty — first healthy in declaration order.
		return healthy[0].key, nil
	}
}

// pickRoundRobin selects a candidate using a modular counter stored in Redis.
func (s *Selector) pickRoundRobin(ctx context.Context, scope string, healthy []candidate) (*hostkey.HostKey, error) {
	var idx int64
	err := s.state.WithLock(ctx, []string{roundRobinKey(scope)}, func(ctx context.Context) error {
		var ierr error
		idx, ierr = s.state.Incr(ctx, roundRobinKey(scope), 1)
		if ierr == nil {
			// Refresh 30-day TTL on every increment. Redis reclaims counters for
			// deleted pools without affecting modular-index correctness.
			_ = s.state.Expire(ctx, roundRobinKey(scope), ttlRoundRobin)
		}
		return ierr
	})
	if err != nil {
		idx = 1
	}
	chosen := healthy[(idx-1)%int64(len(healthy))]
	return chosen.key, nil
}

// pickLRU selects the healthy candidate with the oldest last-use timestamp
// (or never-used) and updates its timestamp.
func (s *Selector) pickLRU(ctx context.Context, scope string, healthy []candidate) (*hostkey.HostKey, error) {
	var (
		chosen    *hostkey.HostKey
		chosenTS  int64 = -1 // -1 = not yet set
		neverUsed *hostkey.HostKey
	)

	for _, c := range healthy {
		k := lruKey(scope, c.key.KeyHash)
		raw, err := s.state.Get(ctx, k)
		if err != nil || len(raw) == 0 {
			// Never used — immediately preferred.
			if neverUsed == nil {
				neverUsed = c.key
			}
			continue
		}
		ts, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil {
			if neverUsed == nil {
				neverUsed = c.key
			}
			continue
		}
		if chosen == nil || ts < chosenTS {
			chosen = c.key
			chosenTS = ts
		}
	}

	if neverUsed != nil {
		chosen = neverUsed
	}
	if chosen == nil {
		// Fallback: should not happen given len(healthy) > 0.
		chosen = healthy[0].key
	}

	// Stamp last-use timestamp.
	now := s.clock().UnixMilli()
	k := lruKey(scope, chosen.KeyHash)
	_ = s.state.Set(ctx, k, []byte(strconv.FormatInt(now, 10)), ttlLRU)

	return chosen, nil
}

// PickWithExclude is a convenience wrapper around Pick that accepts an explicit
// exclude list. Callers that always have an exclude slice can use this to avoid
// the variadic syntax.
func (s *Selector) PickWithExclude(ctx context.Context, scope string, algo KeySelection, keys []*hostkey.HostKey, exclude []*hostkey.HostKey) (*hostkey.HostKey, error) {
	return s.Pick(ctx, scope, algo, keys, exclude)
}

// ClearCircuit deletes the circuit-breaker record for a key from the KV
// store. This is best-effort: if the DEL fails the error is returned to the
// caller, who should log a warning but not fail the outer operation.
//
// Use this when a key is permanently deleted from the catalog so that
// orphaned secret_health:* keys do not accumulate in Redis indefinitely (R-8).
func ClearCircuit(ctx context.Context, store kv.Store, keyHash string) error {
	return store.Del(ctx, circuitKey(keyHash))
}

// RecordSuccess transitions a key to CircuitClosed and resets backoff.
func (s *Selector) RecordSuccess(ctx context.Context, keyHash string) {
	now := s.clock()
	prior := s.readRecord(ctx, keyHash)
	rec := CircuitRecord{
		State:          CircuitClosed,
		BackoffStep:    0,
		LastTransition: now,
		Reason:         "", // clear: key is healthy, prior reason no longer relevant
	}
	s.writeRecord(ctx, keyHash, rec)
	s.log.Debug("keypool transition",
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
	if isAnonymous(keyHash) {
		return
	}
	now := s.clock()
	rec := s.readRecord(ctx, keyHash)
	prior := rec.State

	switch kind {
	case FailureUpstreamUnreachable:
		// Host unreachable (dial failure) — not the key's fault. Never cool the
		// key down; the pipeline retries the same host with backoff instead.
		return

	case FailureAuth:
		rec.State = CircuitOpen
		rec.Indefinite = true
		rec.OpenUntil = time.Time{}
		rec.LastTransition = now
		rec.Reason = ReasonUpstreamAuthFailed
		s.writeRecord(ctx, keyHash, rec)
		metrics.ProviderKeyDown(string(rec.Reason))
		s.log.Debug("keypool transition",
			"request_id", reqid.From(ctx),
			"key_hash", keyHash,
			"from_state", stateName(prior),
			"to_state", stateName(rec.State),
			"reason", "401",
			"cooldown_reason", rec.Reason,
			"backoff_step", rec.BackoffStep,
			"open_for_seconds", 0,
		)

	case FailureRateLimitShort:
		// Stay closed; no state change.
		s.log.Debug("keypool transition",
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
		rec.Reason = ReasonUpstreamRateLimited
		s.writeRecord(ctx, keyHash, rec)
		metrics.ProviderKeyDown(string(rec.Reason))
		s.log.Debug("keypool transition",
			"request_id", reqid.From(ctx),
			"key_hash", keyHash,
			"from_state", stateName(prior),
			"to_state", stateName(rec.State),
			"reason", "rate_limit_long",
			"cooldown_reason", rec.Reason,
			"backoff_step", rec.BackoffStep,
			"open_for_seconds", int(retryAfter.Seconds()),
		)

	case FailureServerError, FailureNetwork:
		logReason := "5xx"
		cooldownReason := ReasonUpstreamServerError
		if kind == FailureNetwork {
			logReason = "network"
			cooldownReason = ReasonUpstreamNetworkError
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
		rec.Reason = cooldownReason
		s.writeRecord(ctx, keyHash, rec)
		metrics.ProviderKeyDown(string(rec.Reason))
		s.log.Debug("keypool transition",
			"request_id", reqid.From(ctx),
			"key_hash", keyHash,
			"from_state", stateName(prior),
			"to_state", stateName(rec.State),
			"reason", logReason,
			"cooldown_reason", rec.Reason,
			"backoff_step", rec.BackoffStep,
			"open_for_seconds", int(dur.Seconds()),
		)
	}
}

// RecordLocalRateLimit cools down a key because our own rate-limit rule
// fired (KeyQuotaExhausted from pkg/ratelimit). Distinct from upstream-driven
// cooldowns: no backoff escalation, no half-open probe — the duration is
// deterministic from Retry-After.
//
// Used by the pipeline's post-Pick Reserve path (issue #89, future PR).
func (s *Selector) RecordLocalRateLimit(ctx context.Context, keyHash string, retryAfter time.Duration) {
	if isAnonymous(keyHash) {
		return
	}
	now := s.clock()
	rec := s.readRecord(ctx, keyHash)
	prior := rec.State
	// Preserve existing BackoffStep — local RL is deterministic, not a sign
	// of key health degradation; don't escalate the backoff ladder.
	rec.State = CircuitOpen
	rec.OpenUntil = now.Add(retryAfter)
	rec.Indefinite = false
	rec.LastTransition = now
	rec.Reason = ReasonLocalRateLimited
	s.writeRecord(ctx, keyHash, rec)
	metrics.ProviderKeyDown(string(rec.Reason))
	s.log.Debug("keypool transition",
		"request_id", reqid.From(ctx),
		"key_hash", keyHash,
		"from_state", stateName(prior),
		"to_state", stateName(rec.State),
		"reason", "local_rl_exhausted",
		"cooldown_reason", rec.Reason,
		"backoff_step", rec.BackoffStep,
		"open_for_seconds", int(retryAfter.Seconds()),
	)
}
