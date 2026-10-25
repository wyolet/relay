package keypool

import "time"

// The functions here are the breaker's transitions. The Lua scripts in scripts.go run the same logic server-side; the Mem emulators and the no-script fallback call these directly, so a change to one must land in both.

// failureOp is one failure to apply to a record.
type failureOp struct {
	now    time.Time
	reason CooldownReason
	// threshold is how many failures within window open a closed key; 0 means the failure is not counted and opens it at once.
	threshold int
	window    time.Duration
	// ladder is the open time per backoff step; empty means open for openFor without touching the step.
	ladder  []time.Duration
	openFor time.Duration
}

// applyFailure returns rec after the failure and whether it changed. A key already open stays as it is for a counted failure (a request picked before the key opened) or a repeat of the reason it is open for, so in-flight stragglers do not climb the ladder.
func applyFailure(rec CircuitRecord, op failureOp) (CircuitRecord, bool) {
	if len(op.ladder) == 0 {
		return openRecord(rec, op, op.now.Add(op.openFor)), true
	}
	if rec.State == CircuitOpen && (op.threshold > 0 || rec.Reason == op.reason) {
		return rec, false
	}
	if rec.State == CircuitClosed && op.threshold > 0 {
		if rec.WindowStart.IsZero() || op.now.Sub(rec.WindowStart) >= op.window {
			rec.WindowStart = op.now
			rec.Failures = 0
		}
		rec.Failures++
		if rec.Failures < op.threshold {
			return rec, true
		}
	}
	rec.BackoffStep = min(rec.BackoffStep+1, len(op.ladder))
	return openRecord(rec, op, op.now.Add(op.ladder[rec.BackoffStep-1])), true
}

func openRecord(rec CircuitRecord, op failureOp, until time.Time) CircuitRecord {
	rec.State = CircuitOpen
	rec.OpenUntil = until
	rec.LastTransition = op.now
	rec.Reason = op.reason
	rec.Indefinite = false
	rec.Failures = 0
	rec.WindowStart = time.Time{}
	rec.ProbeUntil = time.Time{}
	return rec
}

// applySuccess closes a closed or half-open record, one backoff step down, and clears its failure count. An open record is left alone: the success came from a request picked before the key opened.
func applySuccess(rec CircuitRecord, now time.Time) (CircuitRecord, bool) {
	if rec.State == CircuitOpen {
		return rec, false
	}
	if rec.State != CircuitClosed {
		rec.LastTransition = now
	}
	rec.State = CircuitClosed
	rec.BackoffStep = max(rec.BackoffStep-1, 0)
	rec.Reason = ""
	rec.Indefinite = false
	rec.Failures = 0
	rec.WindowStart = time.Time{}
	rec.ProbeUntil = time.Time{}
	return rec, true
}

// claimProbe takes the probe lease on a key whose cooldown has ended (open past OpenUntil, or half-open with a lapsed lease). It reports false when another pick holds the lease or the key is still cooling down.
func claimProbe(rec CircuitRecord, now time.Time, lease time.Duration) (CircuitRecord, bool) {
	switch rec.State {
	case CircuitOpen:
		if now.Before(rec.OpenUntil) {
			return rec, false
		}
		rec.LastTransition = now
	case CircuitHalfOpen:
		if now.Before(rec.ProbeUntil) {
			return rec, false
		}
	default:
		return rec, false
	}
	rec.State = CircuitHalfOpen
	rec.Indefinite = false
	rec.ProbeUntil = now.Add(lease)
	return rec, true
}
