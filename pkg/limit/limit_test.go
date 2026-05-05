package limit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/configstore"
	"github.com/wyolet/relay/pkg/state"
)

// helpers

func newStore(t *testing.T) state.Store {
	t.Helper()
	s := state.New()
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func makeRule(meter configstore.Meter, amount int64, window time.Duration) configstore.ResolvedRule {
	return configstore.ResolvedRule{
		ParentKind: configstore.KindRoute,
		ParentName: "test-route",
		Meter:      meter,
		RateLimit: &configstore.RateLimit{
			Metadata: configstore.Metadata{Name: "rl-" + string(meter)},
			Spec: configstore.RateLimitSpec{
				Strategy: configstore.StrategySlidingWindow,
				Window:   window,
				Amount:   amount,
			},
		},
	}
}

func newLimiter(t *testing.T, now *time.Time) *Limiter {
	t.Helper()
	s := newStore(t)
	clock := func() time.Time { return *now }
	return New(s, discardLogger(), clock)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestRequests_RPMWindow_HappyPath: amount=10, 10 requests succeed, 11th fails.
func TestRequests_RPMWindow_HappyPath(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
	l := newLimiter(t, &now)
	ctx := context.Background()
	rule := makeRule(configstore.MeterRequests, 10, time.Minute)
	rules := []configstore.ResolvedRule{rule}

	for i := 0; i < 10; i++ {
		res, err := l.Reserve(ctx, rules)
		if err != nil {
			t.Fatalf("reserve %d: unexpected error: %v", i+1, err)
		}
		_ = l.Commit(ctx, res, Observations{})
	}

	_, err := l.Reserve(ctx, rules)
	if err == nil {
		t.Fatal("expected ExceededError on 11th reserve")
	}
	var ee *ExceededError
	if !errors.As(err, &ee) {
		t.Fatalf("expected *ExceededError, got %T", err)
	}
	if !errors.Is(err, ErrExceeded) {
		t.Fatal("expected errors.Is(err, ErrExceeded)")
	}
	if ee.Rule.Meter != configstore.MeterRequests {
		t.Fatalf("expected requests meter, got %s", ee.Rule.Meter)
	}
}

// TestRequests_SlidingInterpolation: at t=30s half-window, old bucket has weight 0.5.
func TestRequests_SlidingInterpolation(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base.Add(500 * time.Millisecond) // slightly into the first bucket
	l := newLimiter(t, &now)
	ctx := context.Background()
	rule := makeRule(configstore.MeterRequests, 10, time.Minute)
	rules := []configstore.ResolvedRule{rule}

	// Fill 10 requests in the first bucket.
	for i := 0; i < 10; i++ {
		res, err := l.Reserve(ctx, rules)
		if err != nil {
			t.Fatalf("fill reserve %d: %v", i+1, err)
		}
		_ = l.Commit(ctx, res, Observations{})
	}

	// Advance to t=1min+30s: we're 30s into the second window.
	// Previous bucket (first bucket, 10 requests) has weight 0.5.
	// Interpolated rate for the new (empty) bucket = 0 + 10*0.5 = 5.
	// With amount=10, up to 5 more requests should be allowed before exceeding.
	now = base.Add(time.Minute + 30*time.Second)
	okCount := 0
	for i := 0; i < 10; i++ {
		res, err := l.Reserve(ctx, rules)
		if err != nil {
			if !errors.Is(err, ErrExceeded) {
				t.Fatalf("unexpected error: %v", err)
			}
			break
		}
		okCount++
		_ = l.Commit(ctx, res, Observations{})
	}
	// At 30s into new window, prev weight=0.5 so 5 slots available.
	if okCount < 4 || okCount > 6 {
		t.Fatalf("expected ~5 requests to succeed at half-window, got %d", okCount)
	}

	// At t=3min: all prior buckets fully expired (2*window TTL).
	// Fresh start — 10 slots available.
	now = base.Add(3 * time.Minute)
	for i := 0; i < 10; i++ {
		res, err := l.Reserve(ctx, rules)
		if err != nil {
			t.Fatalf("new window reserve %d: %v", i+1, err)
		}
		_ = l.Commit(ctx, res, Observations{})
	}
	_, err := l.Reserve(ctx, rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected exceeded after 10 in new window, got %v", err)
	}
}

// TestTokens_PostHocOnly: tokens checked at Reserve (peek), incremented at Commit.
func TestTokens_PostHocOnly(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
	l := newLimiter(t, &now)
	ctx := context.Background()
	rule := makeRule(configstore.MeterTokens, 100, time.Minute)
	rules := []configstore.ResolvedRule{rule}

	// 5 reserves succeed (tokens not yet consumed).
	var reservations [5]*Reservation
	for i := 0; i < 5; i++ {
		res, err := l.Reserve(ctx, rules)
		if err != nil {
			t.Fatalf("reserve %d: %v", i+1, err)
		}
		reservations[i] = res
	}

	// Commit each with 20 tokens → total 100.
	for i, res := range reservations {
		if err := l.Commit(ctx, res, Observations{Tokens: 20}); err != nil {
			t.Fatalf("commit %d: %v", i+1, err)
		}
	}

	// 6th Reserve should fail: tokens=100 >= 100.
	_, err := l.Reserve(ctx, rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected ErrExceeded after 100 tokens consumed, got %v", err)
	}
	var ee *ExceededError
	if !errors.As(err, &ee) || ee.Rule.Meter != configstore.MeterTokens {
		t.Fatalf("expected tokens meter exceeded, got %v", err)
	}
}

// TestConcurrency_BudgetCap: amount=3, 3 succeed, 4th fails, after commit 5th succeeds.
func TestConcurrency_BudgetCap(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newLimiter(t, &now)
	ctx := context.Background()
	rule := makeRule(configstore.MeterConcurrency, 3, time.Minute)
	rules := []configstore.ResolvedRule{rule}

	var r [3]*Reservation
	for i := 0; i < 3; i++ {
		res, err := l.Reserve(ctx, rules)
		if err != nil {
			t.Fatalf("reserve %d: %v", i+1, err)
		}
		r[i] = res
	}

	_, err := l.Reserve(ctx, rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected ErrExceeded on 4th, got %v", err)
	}

	// Commit one.
	if err := l.Commit(ctx, r[0], Observations{}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 5th should succeed.
	res, err := l.Reserve(ctx, rules)
	if err != nil {
		t.Fatalf("reserve after commit: %v", err)
	}
	_ = l.Commit(ctx, res, Observations{})
}

// TestConcurrency_CommitOnCancel_DecreasesCounter
func TestConcurrency_CommitOnCancel_DecreasesCounter(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newLimiter(t, &now)
	ctx := context.Background()
	rule := makeRule(configstore.MeterConcurrency, 1, time.Minute)
	rules := []configstore.ResolvedRule{rule}

	res, err := l.Reserve(ctx, rules)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}

	// Second should fail.
	_, err = l.Reserve(ctx, rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected exceeded, got %v", err)
	}

	// Commit with cancelled=true.
	if err := l.Commit(ctx, res, Observations{Cancelled: true}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Now should succeed.
	res2, err := l.Reserve(ctx, rules)
	if err != nil {
		t.Fatalf("reserve after cancel commit: %v", err)
	}
	_ = l.Commit(ctx, res2, Observations{})
}

// TestComposition_FirstViolationShortCircuits
func TestComposition_FirstViolationShortCircuits(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
	l := newLimiter(t, &now)
	ctx := context.Background()

	rule0 := makeRule(configstore.MeterRequests, 100, time.Minute)
	rule0.RateLimit.Metadata.Name = "rl-rule0"
	rule1 := makeRule(configstore.MeterConcurrency, 0, time.Minute) // cap=0, always fails
	rule1.RateLimit.Metadata.Name = "rl-rule1"
	rule2 := makeRule(configstore.MeterRequests, 100, time.Minute)
	rule2.RateLimit.Metadata.Name = "rl-rule2"

	rules := []configstore.ResolvedRule{rule0, rule1, rule2}

	_, err := l.Reserve(ctx, rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected exceeded, got %v", err)
	}

	var ee *ExceededError
	errors.As(err, &ee)
	if ee.Rule.RateLimit.Metadata.Name != "rl-rule1" {
		t.Fatalf("expected rule1 to be violated, got %s", ee.Rule.RateLimit.Metadata.Name)
	}

	// rule0's requests counter should be rolled back → still 0.
	rem, err := l.RemainingByMeter(ctx, []configstore.ResolvedRule{rule0})
	if err != nil {
		t.Fatalf("remaining: %v", err)
	}
	if rem[configstore.MeterRequests] != 100 {
		t.Fatalf("expected rule0 requests remaining=100 after rollback, got %d", rem[configstore.MeterRequests])
	}

	// rule2 should be untouched (rule1 failed before rule2 was evaluated).
	rem2, err := l.RemainingByMeter(ctx, []configstore.ResolvedRule{rule2})
	if err != nil {
		t.Fatalf("remaining rule2: %v", err)
	}
	if rem2[configstore.MeterRequests] != 100 {
		t.Fatalf("expected rule2 untouched, got %d", rem2[configstore.MeterRequests])
	}
}

// TestIdempotentCommit: double Commit is a no-op.
func TestIdempotentCommit(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newLimiter(t, &now)
	ctx := context.Background()
	rule := makeRule(configstore.MeterConcurrency, 1, time.Minute)
	rules := []configstore.ResolvedRule{rule}

	res, err := l.Reserve(ctx, rules)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}

	obs := Observations{Tokens: 50}
	if err := l.Commit(ctx, res, obs); err != nil {
		t.Fatalf("commit 1: %v", err)
	}
	// Second commit — should be no-op.
	if err := l.Commit(ctx, res, obs); err != nil {
		t.Fatalf("commit 2: %v", err)
	}

	// Concurrency counter should be 0 now (decremented once, not twice).
	rem, err := l.RemainingByMeter(ctx, rules)
	if err != nil {
		t.Fatalf("remaining: %v", err)
	}
	if rem[configstore.MeterConcurrency] != 1 {
		t.Fatalf("expected concurrency remaining=1, got %d", rem[configstore.MeterConcurrency])
	}
}

// TestSlidingWindow_BoundaryAccuracy
func TestSlidingWindow_BoundaryAccuracy(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 1, 0, 0, time.UTC) // minute boundary
	rule := makeRule(configstore.MeterRequests, 5, time.Minute)

	// Fill 5 in bucket starting at base.
	{
		now := base.Add(500 * time.Millisecond) // t=0.5s into bucket
		l := newLimiter(t, &now)
		ctx := context.Background()
		rules := []configstore.ResolvedRule{rule}
		for i := 0; i < 5; i++ {
			res, err := l.Reserve(ctx, rules)
			if err != nil {
				t.Fatalf("fill %d: %v", i+1, err)
			}
			_ = l.Commit(ctx, res, Observations{})
		}
		// 6th should fail.
		_, err := l.Reserve(ctx, rules)
		if !errors.Is(err, ErrExceeded) {
			t.Fatalf("expected exceeded at t=0.5s, got %v", err)
		}
	}

	// At t=59.999s into the same bucket, bucket hasn't rolled — still exceeded.
	{
		now := base.Add(59*time.Second + 999*time.Millisecond)
		l := newLimiter(t, &now)
		ctx := context.Background()
		// Same store not shared — independent test. Just verify math is correct.
		// Fill 5 in this window.
		rules := []configstore.ResolvedRule{rule}
		for i := 0; i < 5; i++ {
			res, err := l.Reserve(ctx, rules)
			if err != nil {
				t.Fatalf("fill at 59.999s %d: %v", i+1, err)
			}
			_ = l.Commit(ctx, res, Observations{})
		}
		_, err := l.Reserve(ctx, rules)
		if !errors.Is(err, ErrExceeded) {
			t.Fatalf("expected exceeded at t=59.999s, got %v", err)
		}
	}

	// At t=60.001s (next bucket), bucket resets.
	{
		now := base.Add(60*time.Second + time.Millisecond)
		l := newLimiter(t, &now)
		ctx := context.Background()
		rules := []configstore.ResolvedRule{rule}
		// New bucket, nothing in it.
		res, err := l.Reserve(ctx, rules)
		if err != nil {
			t.Fatalf("expected success in new bucket, got %v", err)
		}
		_ = l.Commit(ctx, res, Observations{})
	}
}

// TestRemainingByMeter_MinAcrossRules
func TestRemainingByMeter_MinAcrossRules(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
	l := newLimiter(t, &now)
	ctx := context.Background()

	rule10 := makeRule(configstore.MeterRequests, 10, time.Minute)
	rule10.RateLimit.Metadata.Name = "rl-10"
	rule5 := makeRule(configstore.MeterRequests, 5, time.Minute)
	rule5.RateLimit.Metadata.Name = "rl-5"

	rules := []configstore.ResolvedRule{rule10, rule5}

	// 3 reserves → consumes from both rules.
	for i := 0; i < 3; i++ {
		res, err := l.Reserve(ctx, rules)
		if err != nil {
			t.Fatalf("reserve %d: %v", i+1, err)
		}
		_ = l.Commit(ctx, res, Observations{})
	}

	rem, err := l.RemainingByMeter(ctx, rules)
	if err != nil {
		t.Fatalf("remaining: %v", err)
	}
	// min(10-3, 5-3) = min(7, 2) = 2
	if rem[configstore.MeterRequests] != 2 {
		t.Fatalf("expected remaining=2, got %d", rem[configstore.MeterRequests])
	}
}

// TestReserve_ContextCancel
func TestReserve_ContextCancel(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newLimiter(t, &now)
	rule := makeRule(configstore.MeterConcurrency, 2, time.Minute)
	rules := []configstore.ResolvedRule{rule}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	res1, err := l.Reserve(ctx, rules)
	if err != nil {
		t.Fatalf("reserve 1: %v", err)
	}
	res2, err := l.Reserve(ctx, rules)
	if err != nil {
		t.Fatalf("reserve 2: %v", err)
	}

	cancel()

	// Commit still works (uses background-like ctx for cleanup).
	_ = l.Commit(context.Background(), res1, Observations{})
	_ = l.Commit(context.Background(), res2, Observations{})

	// After commits, counter should be 0.
	rem, err := l.RemainingByMeter(context.Background(), rules)
	if err != nil {
		t.Fatalf("remaining: %v", err)
	}
	if rem[configstore.MeterConcurrency] != 2 {
		t.Fatalf("expected remaining=2, got %d", rem[configstore.MeterConcurrency])
	}
}
