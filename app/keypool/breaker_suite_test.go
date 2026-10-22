package keypool

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
)

// runBreakerSuite pins the breaker transitions on a backend; the same cases run against kv.Mem and Redis so the Lua scripts and their Go mirrors cannot drift.
func runBreakerSuite(t *testing.T, factory pickStoreFactory) {
	ctx := context.Background()
	newSelector := func(t *testing.T, store kv.Store) *Selector {
		return New(store, noopLogger(), frozenClock(t0), nil)
	}
	at := func(sel *Selector, d time.Duration) { sel.clock = frozenClock(t0.Add(d)) }

	t.Run("threshold_within_window", func(t *testing.T) {
		sel := newSelector(t, factory(t)).WithBreaker(3, 10*time.Second)
		k := "h-threshold"
		sel.RecordFailure(ctx, k, FailureServerError, 0)
		sel.RecordFailure(ctx, k, FailureServerError, 0)
		// The window has passed: the count starts over, so the third failure does not open the key.
		at(sel, 10*time.Second)
		sel.RecordFailure(ctx, k, FailureServerError, 0)
		if rec := sel.readRecord(ctx, k); rec.State != CircuitClosed || rec.Failures != 1 {
			t.Fatalf("after the window: want closed with 1 failure, got %+v", rec)
		}
		sel.RecordFailure(ctx, k, FailureServerError, 0)
		sel.RecordFailure(ctx, k, FailureNetwork, 0)
		rec := sel.readRecord(ctx, k)
		if rec.State != CircuitOpen || rec.BackoffStep != 1 || rec.Reason != ReasonUpstreamNetworkError || rec.Failures != 0 {
			t.Fatalf("at the threshold: want open at step 1, got %+v", rec)
		}
		if want := t0.Add(10*time.Second + backoffSchedule[0]); !rec.OpenUntil.Equal(want) {
			t.Fatalf("OpenUntil = %v, want %v", rec.OpenUntil, want)
		}
	})

	t.Run("success_clears_count", func(t *testing.T) {
		sel := newSelector(t, factory(t)).WithBreaker(2, time.Minute)
		k := "h-success-count"
		sel.RecordFailure(ctx, k, FailureServerError, 0)
		c, err := sel.PickCandidate(ctx, "p", KeySelectionPrioritized, []*hostkey.HostKey{key("s", k)}, nil)
		if err != nil || !c.SuccessNeedsWrite() {
			t.Fatalf("a key with a failure count must take a success write, got %+v, %v", c, err)
		}
		sel.RecordSuccess(ctx, k)
		sel.RecordFailure(ctx, k, FailureServerError, 0)
		if rec := sel.readRecord(ctx, k); rec.State != CircuitClosed || rec.Failures != 1 {
			t.Fatalf("a success between failures restarts the count, got %+v", rec)
		}
	})

	t.Run("open_key_ignores_stragglers", func(t *testing.T) {
		sel := newSelector(t, factory(t)).WithBreaker(1, time.Minute)
		k := "h-straggler"
		sel.RecordFailure(ctx, k, FailureServerError, 0)
		sel.RecordFailure(ctx, k, FailureServerError, 0)
		sel.RecordFailure(ctx, k, FailureNetwork, 0)
		if rec := sel.readRecord(ctx, k); rec.BackoffStep != 1 || rec.Reason != ReasonUpstreamServerError {
			t.Fatalf("counted failures on an open key must not climb the ladder, got %+v", rec)
		}
		// A rejected credential is a new reason: it opens the key on the auth ladder.
		sel.RecordFailure(ctx, k, FailureAuth, 0)
		sel.RecordFailure(ctx, k, FailureAuth, 0)
		rec := sel.readRecord(ctx, k)
		if rec.Reason != ReasonUpstreamAuthFailed || rec.BackoffStep != 2 || !rec.OpenUntil.Equal(t0.Add(authCooldowns[1])) {
			t.Fatalf("want one auth step on top, got %+v", rec)
		}
	})

	t.Run("success_leaves_open_key", func(t *testing.T) {
		sel := newSelector(t, factory(t))
		k := "h-open-success"
		sel.RecordFailure(ctx, k, FailureAuth, 0)
		sel.RecordSuccess(ctx, k)
		if rec := sel.readRecord(ctx, k); rec.State != CircuitOpen || rec.Reason != ReasonUpstreamAuthFailed {
			t.Fatalf("a success from before the key opened must not close it, got %+v", rec)
		}
	})

	t.Run("one_probe_per_lease", func(t *testing.T) {
		sel := newSelector(t, factory(t))
		k := "h-probe"
		keys := []*hostkey.HostKey{key("s", k)}
		sel.RecordFailure(ctx, k, FailureForbidden, 0)
		at(sel, backoffSchedule[0])
		c, err := sel.PickCandidate(ctx, "p", KeySelectionPrioritized, keys, nil)
		if err != nil || c.Record.State != CircuitHalfOpen || !c.SuccessNeedsWrite() {
			t.Fatalf("first pick after the cooldown: want the probe, got %+v, %v", c, err)
		}
		if _, err := sel.Pick(ctx, "p", KeySelectionPrioritized, keys); err != ErrNoHealthyKeys {
			t.Fatalf("pick during the probe: want ErrNoHealthyKeys, got %v", err)
		}
		at(sel, backoffSchedule[0]+probeLease)
		if _, err := sel.Pick(ctx, "p", KeySelectionPrioritized, keys); err != nil {
			t.Fatalf("pick after the lease lapsed: want another probe, got %v", err)
		}
		sel.RecordSuccess(ctx, k)
		if rec := sel.readRecord(ctx, k); rec.State != CircuitClosed || rec.BackoffStep != 0 || !rec.ProbeUntil.IsZero() {
			t.Fatalf("after the probe's success: want closed, got %+v", rec)
		}
	})

	t.Run("pods_share_the_count", func(t *testing.T) {
		const threshold, pods = 4, 2
		store := factory(t)
		sels := make([]*Selector, pods)
		for i := range sels {
			sels[i] = newSelector(t, store).WithBreaker(threshold, time.Minute)
		}
		k := "h-pods"
		var wg sync.WaitGroup
		for _, sel := range sels {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range threshold - 1 {
					sel.RecordFailure(ctx, k, FailureServerError, 0)
				}
			}()
		}
		wg.Wait()
		// 6 failures against a threshold of 4: the key opens once, and the two after that are stragglers.
		if rec := sels[0].readRecord(ctx, k); rec.State != CircuitOpen || rec.BackoffStep != 1 {
			t.Fatalf("want open at step 1, got %+v", rec)
		}
	})

	t.Run("concurrent_picks_one_probe", func(t *testing.T) {
		store := factory(t)
		sel := newSelector(t, store)
		k := "h-race"
		keys := []*hostkey.HostKey{key("s", k)}
		sel.RecordFailure(ctx, k, FailureForbidden, 0)
		at(sel, backoffSchedule[0])
		var probes atomic.Int64
		var wg sync.WaitGroup
		for range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := sel.Pick(ctx, "p", KeySelectionPrioritized, keys); err == nil {
					probes.Add(1)
				}
			}()
		}
		wg.Wait()
		if n := probes.Load(); n != 1 {
			t.Fatalf("%d concurrent picks probed the key, want 1", n)
		}
	})
}

func TestBreakerMem(t *testing.T) {
	runBreakerSuite(t, func(t *testing.T) kvtest.ScriptStore {
		m := kv.NewMem()
		t.Cleanup(func() { _ = m.Close() })
		return m
	})
}
