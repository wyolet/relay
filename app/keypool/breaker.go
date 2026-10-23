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

// backoffSchedule is the open time per step after server errors, network errors and 403s.
var backoffSchedule = [...]time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute}

// authCooldowns is the open time per step after a rejected credential. It is
// bounded so a rejection the caller provoked cannot take a shared key out for
// good; a key that is really revoked is probed at most once per final step.
var authCooldowns = [...]time.Duration{30 * time.Second, time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}

func (s *Selector) readRecord(ctx context.Context, keyHash string) CircuitRecord {
	r, _ := recordFrom(s.state.Get(ctx, circuitKey(keyHash)))
	return r
}

// recordFrom decodes a breaker read and reports whether it held a record. A missing, failed or undecodable read is a closed record, so a store outage never empties the pool.
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

// ReadCircuit returns the stored circuit-breaker record for a key and whether a record actually exists in the state store. A missing or undecodable record yields a default-closed record with found=false — the key has not failed within the record's TTL and is assumed healthy (a success writes a record only to close a half-open probe or reset a backoff). This is a read-only accessor for the admin plane; it does not auto-transition expired-open records the way Pick does.
func (s *Selector) ReadCircuit(ctx context.Context, keyHash string) (CircuitRecord, bool) {
	return recordFrom(s.state.Get(ctx, circuitKey(keyHash)))
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

// RecordSuccess closes a closed or half-open key one backoff step down and clears its failure count, in one script. An open key is left alone.
func (s *Selector) RecordSuccess(ctx context.Context, keyHash string) {
	now := s.clock()
	raw, err := s.runRecordScript(ctx, successCall(keyHash, now), func(rec CircuitRecord, found bool) (CircuitRecord, bool) {
		if !found {
			return rec, false
		}
		return applySuccess(rec, now)
	})
	s.finishSuccess(ctx, keyHash, raw, err)
}

// runRecordScript runs a breaker script. With no script runner it applies the same transition from Go, without the atomicity.
func (s *Selector) runRecordScript(ctx context.Context, call kv.ScriptCall, apply recordTransition) ([]byte, error) {
	if s.runner != nil {
		return s.runner.RunScript(ctx, call.Name, call.Script, call.Keys, call.Args...)
	}
	return applyRecord(ctx, s.state, call.Keys[0], apply, ttlFlat)
}

// finishSuccess logs the outcome of a success script.
func (s *Selector) finishSuccess(ctx context.Context, keyHash string, raw []byte, err error) {
	var out recordOutcome
	if err == nil {
		out, err = decodeOutcome(raw)
	}
	if err != nil {
		metrics.KVError("breaker_write", metrics.KVDropped)
		s.log.Error("keypool: record success script failed", "key_hash", keyHash, "err", err)
		return
	}
	if !out.changed {
		return
	}
	s.log.Debug("keypool transition",
		"request_id", reqid.From(ctx),
		"key_hash", keyHash,
		"from_state", stateName(out.prior),
		"to_state", stateName(out.rec.State),
		"reason", "success",
		"backoff_step", out.rec.BackoffStep,
		"open_for_seconds", 0,
	)
}

// RecordFailure transitions according to kind, in one script. Server and network errors open a closed key only once the threshold is reached within the window; 401 and 403 open it at once. retryAfter is honoured only for FailureRateLimitLong.
func (s *Selector) RecordFailure(ctx context.Context, keyHash string, kind FailureKind, retryAfter time.Duration) {
	if isAnonymous(keyHash) {
		return
	}
	op := failureOp{now: s.clock()}
	var logReason string
	switch kind {
	case FailureUpstreamUnreachable:
		// Host unreachable (dial failure) — not the key's fault. Never cool the
		// key down; the pipeline retries the same host with backoff instead.
		return
	case FailureRateLimitShort:
		// Stay closed; no state change.
		s.log.Debug("keypool transition",
			"request_id", reqid.From(ctx),
			"key_hash", keyHash,
			"reason", "rate_limit_short",
			"open_for_seconds", 0,
		)
		return
	case FailureAuth:
		op.reason, op.ladder, logReason = ReasonUpstreamAuthFailed, authCooldowns[:], "401"
	case FailureForbidden:
		op.reason, op.ladder, logReason = ReasonUpstreamForbidden, backoffSchedule[:], "403"
	case FailureRateLimitLong:
		op.reason, op.openFor, logReason = ReasonUpstreamRateLimited, retryAfter, "rate_limit_long"
	case FailureServerError, FailureNetwork:
		op.reason, logReason = ReasonUpstreamServerError, "5xx"
		if kind == FailureNetwork {
			op.reason, logReason = ReasonUpstreamNetworkError, "network"
		}
		op.ladder, op.threshold, op.window = backoffSchedule[:], s.failureThreshold, s.failureWindow
	default:
		return
	}
	s.recordFailure(ctx, keyHash, op, logReason)
}

func (s *Selector) recordFailure(ctx context.Context, keyHash string, op failureOp, logReason string) {
	// The failure happened whether or not the caller is still there, so its cancellation must not drop the write.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	raw, err := s.runRecordScript(ctx, failureCall(keyHash, op), func(rec CircuitRecord, _ bool) (CircuitRecord, bool) {
		return applyFailure(rec, op)
	})
	var out recordOutcome
	if err == nil {
		out, err = decodeOutcome(raw)
	}
	if err != nil {
		metrics.KVError("breaker_write", metrics.KVDropped)
		s.log.Error("keypool: record failure script failed", "key_hash", keyHash, "err", err)
		return
	}
	openFor := 0
	if out.changed && out.rec.State == CircuitOpen {
		metrics.ProviderKeyDown(string(out.rec.Reason))
		openFor = int(out.rec.OpenUntil.Sub(op.now).Seconds())
	}
	s.log.Debug("keypool transition",
		"request_id", reqid.From(ctx),
		"key_hash", keyHash,
		"from_state", stateName(out.prior),
		"to_state", stateName(out.rec.State),
		"reason", logReason,
		"cooldown_reason", out.rec.Reason,
		"backoff_step", out.rec.BackoffStep,
		"failures", out.rec.Failures,
		"open_for_seconds", openFor,
	)
}
