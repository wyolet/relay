package keypool

import (
	"context"
	"errors"
	"time"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/metrics"
	"github.com/wyolet/relay/pkg/reqid"
)

// FailureKind classifies the upstream failure for circuit-breaker transitions.
type FailureKind int

const (
	FailureAuth           FailureKind = iota // 401 → open on the auth cooldown ladder
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
	// FailureForbidden is a 403: the credential authenticated but the request
	// was refused, often for what it asked (a model or feature the account
	// lacks), so it backs off like a 5xx rather than condemning the key.
	FailureForbidden
)

// CircuitState describes the current health of a key.
type CircuitState int

const (
	CircuitClosed   CircuitState = iota // healthy, accepting traffic
	CircuitOpen                         // unhealthy, skip
	CircuitHalfOpen                     // single probe allowed
)

// backoffSchedule is seconds per step, capped at 60.
var backoffSchedule = [7]int{1, 2, 4, 8, 16, 32, 60}

// authCooldowns is the open time per step after a rejected credential. It is
// bounded so a rejection the caller provoked cannot take a shared key out for
// good; a key that is really revoked is probed at most once per final step.
var authCooldowns = [5]time.Duration{30 * time.Second, time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}

func (s *Selector) readRecord(ctx context.Context, keyHash string) CircuitRecord {
	r, _ := recordFrom(s.state.Get(ctx, circuitKey(keyHash)))
	return r
}

// recordFrom decodes a breaker read and reports whether it held a record. A
// missing, failed or undecodable read is a closed record, so a store outage
// never empties the pool.
func recordFrom(b []byte, err error) (CircuitRecord, bool) {
	if err != nil && !errors.Is(err, kv.ErrNotFound) {
		metrics.KVError("breaker_read", metrics.KVFallback)
	}
	if err != nil || len(b) == 0 {
		return CircuitRecord{State: CircuitClosed}, false
	}
	r, err := decodeRecord(b)
	if err != nil {
		return CircuitRecord{State: CircuitClosed}, false
	}
	return r, true
}

// ReadCircuit returns the stored circuit-breaker record for a key and whether
// a record actually exists in the state store. A missing or undecodable record
// yields a default-closed record with found=false — the key has not failed
// within the record's TTL and is assumed healthy (a success writes a record
// only to close a half-open probe or reset a backoff). This is a read-only
// accessor for the admin plane; it does not auto-transition expired-open
// records the way Pick does.
func (s *Selector) ReadCircuit(ctx context.Context, keyHash string) (CircuitRecord, bool) {
	return recordFrom(s.state.Get(ctx, circuitKey(keyHash)))
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
		metrics.KVError("breaker_write", metrics.KVDropped)
		s.log.Error("keypool: write record failed", "key_hash", keyHash, "err", err)
	}
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
//
// The read (prior state, for logging) and the write happen atomically in a
// single Lua round trip so a concurrent RecordFailure on another pod cannot be
// lost between our GET and SET. Falls back to the non-atomic Go-side GET+SET
// only when the store has no script runner (custom test doubles).
func (s *Selector) RecordSuccess(ctx context.Context, keyHash string) {
	if s.runner == nil {
		prior := s.readRecord(ctx, keyHash).State
		s.writeRecord(ctx, keyHash, successRecord(s.clock()))
		s.logSuccess(ctx, keyHash, prior)
		return
	}
	call, err := s.successCall(keyHash)
	if err != nil {
		s.log.Error("keypool: encode record failed", "key_hash", keyHash, "err", err)
		return
	}
	raw, err := s.runner.RunScript(ctx, call.Name, call.Script, call.Keys, call.Args...)
	s.finishSuccess(ctx, keyHash, raw, err)
}

func successRecord(now time.Time) CircuitRecord {
	// Reason cleared: the key is healthy, the prior reason no longer applies.
	return CircuitRecord{State: CircuitClosed, LastTransition: now}
}

// successCall is RecordSuccess's script call for keyHash.
func (s *Selector) successCall(keyHash string) (kv.ScriptCall, error) {
	b, err := encodeRecord(successRecord(s.clock()))
	if err != nil {
		return kv.ScriptCall{}, err
	}
	return kv.ScriptCall{
		Name:   scriptRecordSuccess,
		Script: recordSuccessScript,
		Keys:   []string{circuitKey(keyHash)},
		Args:   []any{string(b), ttlFlat.Milliseconds()},
	}, nil
}

// finishSuccess logs the outcome of a success script; raw is the prior record.
func (s *Selector) finishSuccess(ctx context.Context, keyHash string, raw []byte, err error) {
	if err != nil {
		metrics.KVError("breaker_write", metrics.KVDropped)
		s.log.Error("keypool: record success script failed", "key_hash", keyHash, "err", err)
		return
	}
	prior := CircuitState(CircuitClosed)
	if len(raw) > 0 {
		if old, derr := decodeRecord(raw); derr == nil {
			prior = old.State
		}
	}
	s.logSuccess(ctx, keyHash, prior)
}

func (s *Selector) logSuccess(ctx context.Context, keyHash string, prior CircuitState) {
	s.log.Debug("keypool transition",
		"request_id", reqid.From(ctx),
		"key_hash", keyHash,
		"from_state", stateName(prior),
		"to_state", stateName(CircuitClosed),
		"reason", "success",
		"backoff_step", 0,
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
		step := min(rec.BackoffStep+1, len(authCooldowns)-1)
		dur := authCooldowns[step]
		rec.BackoffStep = step
		rec.State = CircuitOpen
		rec.Indefinite = false
		rec.OpenUntil = now.Add(dur)
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
			"open_for_seconds", int(dur.Seconds()),
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

	case FailureServerError, FailureNetwork, FailureForbidden:
		logReason := "5xx"
		cooldownReason := ReasonUpstreamServerError
		switch kind {
		case FailureNetwork:
			logReason = "network"
			cooldownReason = ReasonUpstreamNetworkError
		case FailureForbidden:
			logReason = "403"
			cooldownReason = ReasonUpstreamForbidden
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
