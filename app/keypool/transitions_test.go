package keypool

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestApplyFailure(t *testing.T) {
	server := failureOp{now: t0, reason: ReasonUpstreamServerError, threshold: 3, window: 10 * time.Second, ladder: backoffSchedule[:]}
	auth := failureOp{now: t0, reason: ReasonUpstreamAuthFailed, ladder: authCooldowns[:]}
	rateLimited := failureOp{now: t0, reason: ReasonUpstreamRateLimited, openFor: 45 * time.Second}
	ago := func(d time.Duration) time.Time { return t0.Add(-d) }
	open := func(reason CooldownReason, step int, d time.Duration) CircuitRecord {
		return CircuitRecord{State: CircuitOpen, OpenUntil: t0.Add(d), BackoffStep: step, LastTransition: t0, Reason: reason}
	}
	stillOpen := CircuitRecord{State: CircuitOpen, OpenUntil: t0.Add(time.Minute), BackoffStep: 2, LastTransition: ago(time.Second), Reason: ReasonUpstreamServerError}

	for _, tc := range []struct {
		name    string
		rec     CircuitRecord
		op      failureOp
		want    CircuitRecord
		changed bool
	}{
		{"first counted failure starts the window", CircuitRecord{}, server,
			CircuitRecord{Failures: 1, WindowStart: t0}, true},
		{"counted failure inside the window", CircuitRecord{Failures: 1, WindowStart: ago(10*time.Second - time.Millisecond)}, server,
			CircuitRecord{Failures: 2, WindowStart: ago(10*time.Second - time.Millisecond)}, true},
		{"window over restarts the count", CircuitRecord{Failures: 2, WindowStart: ago(10 * time.Second)}, server,
			CircuitRecord{Failures: 1, WindowStart: t0}, true},
		{"threshold opens at the first step", CircuitRecord{Failures: 2, WindowStart: ago(time.Second)}, server,
			open(ReasonUpstreamServerError, 1, time.Second), true},
		{"threshold opens one step above a decaying backoff", CircuitRecord{BackoffStep: 2, Failures: 2, WindowStart: ago(time.Second)}, server,
			open(ReasonUpstreamServerError, 3, 4*time.Second), true},
		{"failed probe opens at once, a step up", CircuitRecord{State: CircuitHalfOpen, BackoffStep: 1, ProbeUntil: t0.Add(time.Second), Reason: ReasonUpstreamServerError}, server,
			open(ReasonUpstreamServerError, 2, 2*time.Second), true},
		{"backoff step is capped at the ladder's end", CircuitRecord{State: CircuitHalfOpen, BackoffStep: len(backoffSchedule)}, server,
			open(ReasonUpstreamServerError, len(backoffSchedule), time.Minute), true},
		{"counted failure leaves an open key", stillOpen, server, stillOpen, false},
		{"rejected credential opens a closed key at once", CircuitRecord{Failures: 1, WindowStart: ago(time.Second)}, auth,
			open(ReasonUpstreamAuthFailed, 1, 30*time.Second), true},
		{"new reason climbs on an open key", stillOpen, auth,
			open(ReasonUpstreamAuthFailed, 3, 5*time.Minute), true},
		{"repeat of the reason leaves an open key", open(ReasonUpstreamAuthFailed, 1, 30*time.Second), auth,
			open(ReasonUpstreamAuthFailed, 1, 30*time.Second), false},
		{"auth step is capped at its own ladder's end", CircuitRecord{State: CircuitHalfOpen, BackoffStep: len(backoffSchedule)}, auth,
			open(ReasonUpstreamAuthFailed, len(authCooldowns), time.Hour), true},
		{"legacy indefinite flag is dropped", CircuitRecord{State: CircuitHalfOpen, BackoffStep: 1, Indefinite: true}, auth,
			open(ReasonUpstreamAuthFailed, 2, time.Minute), true},
		{"rate limit opens for its retry-after, step untouched", CircuitRecord{BackoffStep: 2, Failures: 1, WindowStart: ago(time.Second)}, rateLimited,
			open(ReasonUpstreamRateLimited, 2, 45*time.Second), true},
		{"rate limit extends an open key", CircuitRecord{State: CircuitOpen, OpenUntil: t0.Add(time.Second), BackoffStep: 2, Reason: ReasonUpstreamServerError}, rateLimited,
			open(ReasonUpstreamRateLimited, 2, 45*time.Second), true},
	} {
		got, changed := applyFailure(tc.rec, tc.op)
		if d := cmp.Diff(tc.want, got); d != "" || changed != tc.changed {
			t.Errorf("%s: changed = %v, want %v (-want +got):\n%s", tc.name, changed, tc.changed, d)
		}
	}
}

func TestApplySuccess(t *testing.T) {
	earlier := t0.Add(-time.Minute)
	open := CircuitRecord{State: CircuitOpen, OpenUntil: t0.Add(time.Second), BackoffStep: 1, Reason: ReasonUpstreamServerError}
	for _, tc := range []struct {
		name    string
		rec     CircuitRecord
		want    CircuitRecord
		changed bool
	}{
		{"open key is left alone", open, open, false},
		{"closed key drops its count and one step", CircuitRecord{BackoffStep: 3, Failures: 2, WindowStart: earlier, LastTransition: earlier},
			CircuitRecord{BackoffStep: 2, LastTransition: earlier}, true},
		{"probe closes the key", CircuitRecord{State: CircuitHalfOpen, OpenUntil: earlier, BackoffStep: 1, LastTransition: earlier, Indefinite: true, Reason: ReasonUpstreamAuthFailed, ProbeUntil: t0.Add(time.Second)},
			CircuitRecord{OpenUntil: earlier, LastTransition: t0}, true},
		{"step never goes below zero", CircuitRecord{}, CircuitRecord{}, true},
		{"unknown state closes", CircuitRecord{State: 9}, CircuitRecord{LastTransition: t0}, true},
	} {
		got, changed := applySuccess(tc.rec, t0)
		if d := cmp.Diff(tc.want, got); d != "" || changed != tc.changed {
			t.Errorf("%s: changed = %v, want %v (-want +got):\n%s", tc.name, changed, tc.changed, d)
		}
	}
}

func TestClaimProbe(t *testing.T) {
	const lease = 10 * time.Second
	earlier := t0.Add(-time.Minute)
	for _, tc := range []struct {
		name    string
		rec     CircuitRecord
		want    CircuitRecord
		claimed bool
	}{
		{"closed key has no probe", CircuitRecord{BackoffStep: 1}, CircuitRecord{BackoffStep: 1}, false},
		{"unknown state has no probe", CircuitRecord{State: 9}, CircuitRecord{State: 9}, false},
		{"open key still cooling", CircuitRecord{State: CircuitOpen, OpenUntil: t0.Add(time.Millisecond)},
			CircuitRecord{State: CircuitOpen, OpenUntil: t0.Add(time.Millisecond)}, false},
		{"open key at its end", CircuitRecord{State: CircuitOpen, OpenUntil: t0, BackoffStep: 2, LastTransition: earlier, Indefinite: true, Reason: ReasonUpstreamAuthFailed},
			CircuitRecord{State: CircuitHalfOpen, OpenUntil: t0, BackoffStep: 2, LastTransition: t0, Reason: ReasonUpstreamAuthFailed, ProbeUntil: t0.Add(lease)}, true},
		{"probe lease still out", CircuitRecord{State: CircuitHalfOpen, ProbeUntil: t0.Add(time.Millisecond)},
			CircuitRecord{State: CircuitHalfOpen, ProbeUntil: t0.Add(time.Millisecond)}, false},
		{"lapsed lease is taken over", CircuitRecord{State: CircuitHalfOpen, LastTransition: earlier, ProbeUntil: t0},
			CircuitRecord{State: CircuitHalfOpen, LastTransition: earlier, ProbeUntil: t0.Add(lease)}, true},
	} {
		got, claimed := claimProbe(tc.rec, t0, lease)
		if d := cmp.Diff(tc.want, got); d != "" || claimed != tc.claimed {
			t.Errorf("%s: claimed = %v, want %v (-want +got):\n%s", tc.name, claimed, tc.claimed, d)
		}
	}
}
