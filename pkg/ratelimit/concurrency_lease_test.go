package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

// droppedScripts fails every script, standing in for a store the commit never reached.
type droppedScripts struct{}

func (droppedScripts) RunScript(context.Context, string, string, []string, ...any) ([]byte, error) {
	return nil, kv.ErrUnavailable
}

// runConcurrencyLeaseSuite pins the lease semantics of the concurrency meter on any backend. The limiter clock scores the leases, so moving it ages them on Redis as on Mem.
func runConcurrencyLeaseSuite(t *testing.T, name string, factory func(t *testing.T, now *time.Time) *Limiter) {
	const window = time.Minute
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	ctx := context.Background()
	refusal := func(t *testing.T, l *Limiter, scope string, rules []Rule) *ExceededError {
		t.Helper()
		_, err := l.Reserve(ctx, scope, rules)
		var ee *ExceededError
		if !errors.As(err, &ee) {
			t.Fatalf("reserve: err = %v, want ErrExceeded", err)
		}
		return ee
	}
	refused := func(t *testing.T, l *Limiter, scope string, rules []Rule) {
		t.Helper()
		_ = refusal(t, l, scope, rules)
	}
	granted := func(t *testing.T, l *Limiter, scope string, rules []Rule) *Reservation {
		t.Helper()
		res, err := l.Reserve(ctx, scope, rules)
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		return res
	}

	// A reserve the store ran but whose reply never reached the caller leaves no Reservation to commit; its lease frees the slot.
	t.Run(name+"/LostReserveFreesAfterLease", func(t *testing.T) {
		now := start
		l := factory(t, &now)
		const scope = "lease-lost-reserve"
		rules := []Rule{conRule("Route:lease:lost-reserve", "concurrency", 1, window)}
		call, err := l.buildReserveCall(scope, rules, "lost-token")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := l.runner.RunScript(ctx, call.Name, call.Script, call.Keys, call.Args...); err != nil {
			t.Fatalf("run reserve: %v", err)
		}
		refused(t, l, scope, rules)
		now = start.Add(window - time.Millisecond)
		refused(t, l, scope, rules)
		now = start.Add(window)
		granted(t, l, scope, rules)
	})

	t.Run(name+"/DroppedCommitFreesAfterLease", func(t *testing.T) {
		now := start
		l := factory(t, &now)
		const scope = "lease-dropped-commit"
		rules := []Rule{conRule("Route:lease:dropped-commit", "concurrency", 1, window)}
		res := granted(t, l, scope, rules)
		runner := l.runner
		l.runner = droppedScripts{}
		if err := l.Commit(ctx, res, Observations{}); err == nil {
			t.Fatal("commit through a dropped store succeeded")
		}
		l.runner = runner
		refused(t, l, scope, rules)
		now = start.Add(window)
		granted(t, l, scope, rules)
	})

	t.Run(name+"/RollbackLeavesNoLease", func(t *testing.T) {
		now := start
		l := factory(t, &now)
		const scope = "lease-rollback"
		con := conRule("Route:lease:rollback-con", "concurrency", 1, window)
		blocked := reqRule("Route:lease:rollback-req", "requests", 0, window)
		if ee := refusal(t, l, scope, []Rule{con, blocked}); ee.Rule.Key != blocked.Key {
			t.Fatalf("refused by %q, want %q", ee.Rule.Key, blocked.Key)
		}
		granted(t, l, scope, []Rule{con})
		refused(t, l, scope, []Rule{con})
	})

	t.Run(name+"/CancelledAndRepeatedCommitRelease", func(t *testing.T) {
		now := start
		l := factory(t, &now)
		const scope = "lease-cancelled"
		rules := []Rule{conRule("Route:lease:cancelled", "concurrency", 1, window)}
		res := granted(t, l, scope, rules)
		for _, obs := range []Observations{{Cancelled: true}, {Cancelled: true}, {}} {
			if err := l.Commit(ctx, res, obs); err != nil {
				t.Fatalf("commit %+v: %v", obs, err)
			}
		}
		granted(t, l, scope, rules)
		refused(t, l, scope, rules)
	})

	// A request running past its lease no longer holds the slot, and its late commit releases only its own lease.
	t.Run(name+"/LongRequestStopsCountingAfterLease", func(t *testing.T) {
		now := start
		l := factory(t, &now)
		const scope = "lease-long-request"
		rules := []Rule{conRule("Route:lease:long-request", "concurrency", 1, window)}
		long := granted(t, l, scope, rules)
		now = start.Add(window + time.Second)
		next := granted(t, l, scope, rules)
		if err := l.Commit(ctx, long, Observations{}); err != nil {
			t.Fatalf("late commit: %v", err)
		}
		refused(t, l, scope, rules)
		if err := l.Commit(ctx, next, Observations{}); err != nil {
			t.Fatalf("commit: %v", err)
		}
		granted(t, l, scope, rules)
	})

	t.Run(name+"/RetryAfterEarliestLease", func(t *testing.T) {
		now := start
		l := factory(t, &now)
		const scope = "lease-retry-after"
		rules := []Rule{conRule("Route:lease:retry-after", "concurrency", 2, window)}
		granted(t, l, scope, rules)
		now = start.Add(20 * time.Second)
		granted(t, l, scope, rules)
		now = start.Add(30 * time.Second)
		if ee := refusal(t, l, scope, rules); ee.RetryAfter != 30*time.Second {
			t.Fatalf("RetryAfter = %v, want 30s (the first lease's expiry)", ee.RetryAfter)
		}
	})

	t.Run(name+"/RetryAfterWindowWhenNoLease", func(t *testing.T) {
		now := start
		l := factory(t, &now)
		rules := []Rule{conRule("Route:lease:retry-none", "concurrency", 0, window)}
		if ee := refusal(t, l, "lease-retry-none", rules); ee.RetryAfter != window {
			t.Fatalf("RetryAfter = %v, want the window", ee.RetryAfter)
		}
	})
}

func TestConcurrencyLease_MemStore(t *testing.T) {
	runConcurrencyLeaseSuite(t, "MemStore", newTestLimiter)
}
