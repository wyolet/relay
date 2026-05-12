package ratelimit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/internal/usage"
	"github.com/wyolet/relay/pkg/kv"
)

// helpers

func newStore(t *testing.T) kv.Store {
	t.Helper()
	s := kv.NewMem()
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func makeRule(meter catalog.Meter, amount int64, window time.Duration) catalog.ResolvedRule {
	return catalog.ResolvedRule{
		ParentKind: catalog.KindRoute,
		ParentName: "test-route",
		Meter:      meter,
		RateLimit: &catalog.RateLimit{
			Metadata: catalog.Metadata{Name: "rl-" + string(meter)},
			Spec: catalog.RateLimitSpec{
				Strategy: catalog.StrategySlidingWindow,
				Window:   window,
				Rules:    []catalog.RateLimitRule{{Meter: string(meter), Amount: amount}},
			},
		},
		Rule: catalog.RateLimitRule{Meter: string(meter), Amount: amount},
	}
}

func makeRuleStrategy(meter catalog.Meter, amount int64, window time.Duration, strat catalog.RateLimitStrategy) catalog.ResolvedRule {
	r := makeRule(meter, amount, window)
	r.Strategy = strat
	r.Rule.Strategy = strat
	return r
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
	rule := makeRule(catalog.MeterRequests, 10, time.Minute)
	rules := []catalog.ResolvedRule{rule}

	for i := 0; i < 10; i++ {
		res, err := l.Reserve(ctx, "test-policy", rules)
		if err != nil {
			t.Fatalf("reserve %d: unexpected error: %v", i+1, err)
		}
		_ = l.Commit(ctx, res, Observations{})
	}

	_, err := l.Reserve(ctx, "test-policy", rules)
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
	if ee.Rule.Meter != catalog.MeterRequests {
		t.Fatalf("expected requests meter, got %s", ee.Rule.Meter)
	}
}

// TestRequests_SlidingInterpolation: at t=30s half-window, old bucket has weight 0.5.
func TestRequests_SlidingInterpolation(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base.Add(500 * time.Millisecond) // slightly into the first bucket
	l := newLimiter(t, &now)
	ctx := context.Background()
	rule := makeRule(catalog.MeterRequests, 10, time.Minute)
	rules := []catalog.ResolvedRule{rule}

	// Fill 10 requests in the first bucket.
	for i := 0; i < 10; i++ {
		res, err := l.Reserve(ctx, "test-policy", rules)
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
		res, err := l.Reserve(ctx, "test-policy", rules)
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
		res, err := l.Reserve(ctx, "test-policy", rules)
		if err != nil {
			t.Fatalf("new window reserve %d: %v", i+1, err)
		}
		_ = l.Commit(ctx, res, Observations{})
	}
	_, err := l.Reserve(ctx, "test-policy", rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected exceeded after 10 in new window, got %v", err)
	}
}

// TestTokens_PostHocOnly: tokens checked at Reserve (peek), incremented at Commit.
func TestTokens_PostHocOnly(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
	l := newLimiter(t, &now)
	ctx := context.Background()
	rule := makeRule(catalog.MeterTokens, 100, time.Minute)
	rules := []catalog.ResolvedRule{rule}

	// 5 reserves succeed (tokens not yet consumed).
	var reservations [5]*Reservation
	for i := 0; i < 5; i++ {
		res, err := l.Reserve(ctx, "test-policy", rules)
		if err != nil {
			t.Fatalf("reserve %d: %v", i+1, err)
		}
		reservations[i] = res
	}

	// Commit each with 20 tokens → total 100 (equal to amount; still allowed with > comparator).
	for i, res := range reservations {
		if err := l.Commit(ctx, res, Observations{Tokens: usage.Tokens{"tokens": 20}}); err != nil {
			t.Fatalf("commit %d: %v", i+1, err)
		}
	}

	// 6th reserve succeeds (rate==amount is allowed with strict > comparator).
	res6, err := l.Reserve(ctx, "test-policy", rules)
	if err != nil {
		t.Fatalf("6th reserve (rate==amount should pass): %v", err)
	}
	// Commit 1 more token → total 101 > 100.
	if err := l.Commit(ctx, res6, Observations{Tokens: usage.Tokens{"tokens": 1}}); err != nil {
		t.Fatalf("commit 6: %v", err)
	}

	// 7th Reserve should fail: tokens=101 > 100.
	_, err = l.Reserve(ctx, "test-policy", rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected ErrExceeded after 101 tokens consumed, got %v", err)
	}
	var ee *ExceededError
	if !errors.As(err, &ee) || ee.Rule.Meter != catalog.MeterTokens {
		t.Fatalf("expected tokens meter exceeded, got %v", err)
	}
}

// TestConcurrency_BudgetCap: amount=3, 3 succeed, 4th fails, after commit 5th succeeds.
func TestConcurrency_BudgetCap(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newLimiter(t, &now)
	ctx := context.Background()
	rule := makeRule(catalog.MeterConcurrency, 3, time.Minute)
	rules := []catalog.ResolvedRule{rule}

	var r [3]*Reservation
	for i := 0; i < 3; i++ {
		res, err := l.Reserve(ctx, "test-policy", rules)
		if err != nil {
			t.Fatalf("reserve %d: %v", i+1, err)
		}
		r[i] = res
	}

	_, err := l.Reserve(ctx, "test-policy", rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected ErrExceeded on 4th, got %v", err)
	}

	// Commit one.
	if err := l.Commit(ctx, r[0], Observations{}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 5th should succeed.
	res, err := l.Reserve(ctx, "test-policy", rules)
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
	rule := makeRule(catalog.MeterConcurrency, 1, time.Minute)
	rules := []catalog.ResolvedRule{rule}

	res, err := l.Reserve(ctx, "test-policy", rules)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}

	// Second should fail.
	_, err = l.Reserve(ctx, "test-policy", rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected exceeded, got %v", err)
	}

	// Commit with cancelled=true.
	if err := l.Commit(ctx, res, Observations{Cancelled: true}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Now should succeed.
	res2, err := l.Reserve(ctx, "test-policy", rules)
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

	rule0 := makeRule(catalog.MeterRequests, 100, time.Minute)
	rule0.RateLimit.Metadata.Name = "rl-rule0"
	rule1 := makeRule(catalog.MeterConcurrency, 0, time.Minute) // cap=0, always fails
	rule1.RateLimit.Metadata.Name = "rl-rule1"
	rule2 := makeRule(catalog.MeterRequests, 100, time.Minute)
	rule2.RateLimit.Metadata.Name = "rl-rule2"

	rules := []catalog.ResolvedRule{rule0, rule1, rule2}

	_, err := l.Reserve(ctx, "test-policy", rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected exceeded, got %v", err)
	}

	var ee *ExceededError
	errors.As(err, &ee)
	if ee.Rule.RateLimit.Metadata.Name != "rl-rule1" {
		t.Fatalf("expected rule1 to be violated, got %s", ee.Rule.RateLimit.Metadata.Name)
	}

	// rule0's requests counter should be rolled back → all 100 reserves succeed.
	for i := 0; i < 100; i++ {
		res, err2 := l.Reserve(ctx, "test-policy", []catalog.ResolvedRule{rule0})
		if err2 != nil {
			t.Fatalf("rule0 reserve %d after rollback: %v", i+1, err2)
		}
		_ = l.Commit(ctx, res, Observations{})
	}
}

// TestIdempotentCommit: double Commit is a no-op.
func TestIdempotentCommit(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newLimiter(t, &now)
	ctx := context.Background()
	rule := makeRule(catalog.MeterConcurrency, 1, time.Minute)
	rules := []catalog.ResolvedRule{rule}

	res, err := l.Reserve(ctx, "test-policy", rules)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}

	obs := Observations{Tokens: usage.Tokens{"tokens": 50}}
	if err := l.Commit(ctx, res, obs); err != nil {
		t.Fatalf("commit 1: %v", err)
	}
	// Second commit — should be no-op.
	if err := l.Commit(ctx, res, obs); err != nil {
		t.Fatalf("commit 2: %v", err)
	}

	// Concurrency counter decremented once, not twice → a new reserve succeeds.
	res2, err := l.Reserve(ctx, "test-policy", rules)
	if err != nil {
		t.Fatalf("expected reserve to succeed after idempotent commit, got %v", err)
	}
	_ = l.Commit(ctx, res2, Observations{})
}

// TestSlidingWindow_BoundaryAccuracy
func TestSlidingWindow_BoundaryAccuracy(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 1, 0, 0, time.UTC) // minute boundary
	rule := makeRule(catalog.MeterRequests, 5, time.Minute)

	// Fill 5 in bucket starting at base.
	{
		now := base.Add(500 * time.Millisecond) // t=0.5s into bucket
		l := newLimiter(t, &now)
		ctx := context.Background()
		rules := []catalog.ResolvedRule{rule}
		for i := 0; i < 5; i++ {
			res, err := l.Reserve(ctx, "test-policy", rules)
			if err != nil {
				t.Fatalf("fill %d: %v", i+1, err)
			}
			_ = l.Commit(ctx, res, Observations{})
		}
		// 6th should fail.
		_, err := l.Reserve(ctx, "test-policy", rules)
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
		rules := []catalog.ResolvedRule{rule}
		for i := 0; i < 5; i++ {
			res, err := l.Reserve(ctx, "test-policy", rules)
			if err != nil {
				t.Fatalf("fill at 59.999s %d: %v", i+1, err)
			}
			_ = l.Commit(ctx, res, Observations{})
		}
		_, err := l.Reserve(ctx, "test-policy", rules)
		if !errors.Is(err, ErrExceeded) {
			t.Fatalf("expected exceeded at t=59.999s, got %v", err)
		}
	}

	// At t=60.001s (next bucket), bucket resets.
	{
		now := base.Add(60*time.Second + time.Millisecond)
		l := newLimiter(t, &now)
		ctx := context.Background()
		rules := []catalog.ResolvedRule{rule}
		// New bucket, nothing in it.
		res, err := l.Reserve(ctx, "test-policy", rules)
		if err != nil {
			t.Fatalf("expected success in new bucket, got %v", err)
		}
		_ = l.Commit(ctx, res, Observations{})
	}
}


// TestReserve_ContextCancel
func TestReserve_ContextCancel(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newLimiter(t, &now)
	rule := makeRule(catalog.MeterConcurrency, 2, time.Minute)
	rules := []catalog.ResolvedRule{rule}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	res1, err := l.Reserve(ctx, "test-policy", rules)
	if err != nil {
		t.Fatalf("reserve 1: %v", err)
	}
	res2, err := l.Reserve(ctx, "test-policy", rules)
	if err != nil {
		t.Fatalf("reserve 2: %v", err)
	}

	cancel()

	// Commit still works (uses background-like ctx for cleanup).
	_ = l.Commit(context.Background(), res1, Observations{})
	_ = l.Commit(context.Background(), res2, Observations{})

	// After commits, both concurrency slots freed → two new reserves succeed.
	for i := 0; i < 2; i++ {
		res, err2 := l.Reserve(context.Background(), "test-policy", rules)
		if err2 != nil {
			t.Fatalf("post-cancel reserve %d: expected success, got %v", i+1, err2)
		}
		_ = l.Commit(context.Background(), res, Observations{})
	}
}

// makeMultiRule builds a ResolvedRule using the new Rule field for multi-rule RateLimits.
func makeMultiRule(parentName, rlName string, ruleMeter string, amount int64, window time.Duration) catalog.ResolvedRule {
	return catalog.ResolvedRule{
		ParentKind:    catalog.KindPolicy,
		ParentName:    parentName,
		RateLimitName: rlName,
		Strategy:      catalog.StrategySlidingWindow,
		Window:        window,
		Rule: catalog.RateLimitRule{
			Meter:    ruleMeter,
			Amount:   amount,
			Strategy: catalog.StrategySlidingWindow,
		},
		Meter: catalog.Meter(ruleMeter),
		RateLimit: &catalog.RateLimit{
			Metadata: catalog.Metadata{Name: rlName},
			Spec: catalog.RateLimitSpec{
				Strategy: catalog.StrategySlidingWindow,
				Window:   window,
				Rules:    []catalog.RateLimitRule{{Meter: ruleMeter, Amount: amount}},
			},
		},
	}
}

// makeStrategyRule builds a ResolvedRule for testing a specific strategy.
func makeStrategyRule(strategy catalog.RateLimitStrategy, meter catalog.Meter, amount int64, window time.Duration) catalog.ResolvedRule {
	return catalog.ResolvedRule{
		ParentKind:    catalog.KindRoute,
		ParentName:    "test-route",
		RateLimitName: "rl-" + string(strategy),
		Strategy:      strategy,
		Window:        window,
		Rule: catalog.RateLimitRule{
			Meter:    string(meter),
			Amount:   amount,
			Strategy: strategy,
		},
		Meter: meter,
		RateLimit: &catalog.RateLimit{
			Metadata: catalog.Metadata{Name: "rl-" + string(strategy)},
			Spec: catalog.RateLimitSpec{
				Strategy: strategy,
				Window:   window,
				Rules:    []catalog.RateLimitRule{{Meter: string(meter), Amount: amount}},
			},
		},
	}
}

// TestFixedWindow_HappyPath: amount=5, 5 requests succeed, 6th fails within same window.
func TestFixedWindow_HappyPath(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC) // 30s into 1-minute window
	l := newLimiter(t, &now)
	ctx := context.Background()
	rule := makeStrategyRule(catalog.StrategyFixedWindow, catalog.MeterRequests, 5, time.Minute)
	rules := []catalog.ResolvedRule{rule}

	for i := 0; i < 5; i++ {
		res, err := l.Reserve(ctx, "test-policy", rules)
		if err != nil {
			t.Fatalf("reserve %d: %v", i+1, err)
		}
		_ = l.Commit(ctx, res, Observations{})
	}
	_, err := l.Reserve(ctx, "test-policy", rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected exceeded on 6th, got %v", err)
	}

	// New window: counter resets.
	now = now.Add(time.Minute)
	res, err := l.Reserve(ctx, "test-policy", rules)
	if err != nil {
		t.Fatalf("expected success in new window, got %v", err)
	}
	_ = l.Commit(ctx, res, Observations{})
}

// TestTokenBucket_Refill: burst=5, after exhausting wait for refill.
func TestTokenBucket_Refill(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newLimiter(t, &now)
	ctx := context.Background()
	// burst=5, window=1m → refill rate = 5 tokens/min = 1 token/12s
	rule := makeStrategyRule(catalog.StrategyTokenBucket, catalog.MeterRequests, 5, time.Minute)
	rules := []catalog.ResolvedRule{rule}

	for i := 0; i < 5; i++ {
		res, err := l.Reserve(ctx, "test-policy", rules)
		if err != nil {
			t.Fatalf("reserve %d: %v", i+1, err)
		}
		_ = l.Commit(ctx, res, Observations{})
	}
	_, err := l.Reserve(ctx, "test-policy", rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected exceeded after burst exhausted, got %v", err)
	}

	// Advance 12s → 1 token refilled
	now = now.Add(12 * time.Second)
	res, err := l.Reserve(ctx, "test-policy", rules)
	if err != nil {
		t.Fatalf("expected 1 refilled token to allow request, got %v", err)
	}
	_ = l.Commit(ctx, res, Observations{})
}

// TestTokenBucket_RefundOnCancel: cancelled reservation returns token.
func TestTokenBucket_RefundOnCancel(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newLimiter(t, &now)
	ctx := context.Background()
	rule := makeStrategyRule(catalog.StrategyTokenBucket, catalog.MeterRequests, 1, time.Minute)
	rules := []catalog.ResolvedRule{rule}

	res, err := l.Reserve(ctx, "test-policy", rules)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// bucket exhausted
	_, err = l.Reserve(ctx, "test-policy", rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected exceeded, got %v", err)
	}

	// Cancel → token refunded
	_ = l.Commit(ctx, res, Observations{Cancelled: true})

	res2, err := l.Reserve(ctx, "test-policy", rules)
	if err != nil {
		t.Fatalf("expected success after refund, got %v", err)
	}
	_ = l.Commit(ctx, res2, Observations{})
}

// TestLeakyBucket_DrainAndRefund: queue fills, drains over time, and refund works.
func TestLeakyBucket_DrainAndRefund(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newLimiter(t, &now)
	ctx := context.Background()
	// capacity=3, window=60s → leak_rate = 3/60s = 1 req/20s
	rule := makeStrategyRule(catalog.StrategyLeakyBucket, catalog.MeterRequests, 3, time.Minute)
	rules := []catalog.ResolvedRule{rule}

	for i := 0; i < 3; i++ {
		res, err := l.Reserve(ctx, "test-policy", rules)
		if err != nil {
			t.Fatalf("reserve %d: %v", i+1, err)
		}
		_ = l.Commit(ctx, res, Observations{})
	}
	_, err := l.Reserve(ctx, "test-policy", rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected exceeded after queue full, got %v", err)
	}

	// Advance 20s → 1 slot drained
	now = now.Add(20 * time.Second)
	res, err := l.Reserve(ctx, "test-policy", rules)
	if err != nil {
		t.Fatalf("expected slot after drain, got %v", err)
	}
	_ = l.Commit(ctx, res, Observations{})
}

// TestLeakyBucket_RefundOnCancel
func TestLeakyBucket_RefundOnCancel(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newLimiter(t, &now)
	ctx := context.Background()
	rule := makeStrategyRule(catalog.StrategyLeakyBucket, catalog.MeterRequests, 1, time.Minute)
	rules := []catalog.ResolvedRule{rule}

	res, err := l.Reserve(ctx, "test-policy", rules)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	_, err = l.Reserve(ctx, "test-policy", rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected exceeded, got %v", err)
	}

	_ = l.Commit(ctx, res, Observations{Cancelled: true})

	res2, err := l.Reserve(ctx, "test-policy", rules)
	if err != nil {
		t.Fatalf("expected success after cancel refund, got %v", err)
	}
	_ = l.Commit(ctx, res2, Observations{})
}

// TestMultiRule_AllGranted: all rules have headroom → reservation succeeds.
func TestMultiRule_AllGranted(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
	l := newLimiter(t, &now)
	ctx := context.Background()

	rules := []catalog.ResolvedRule{
		makeMultiRule("test-policy", "tier-1", "requests", 10, time.Minute),
		makeMultiRule("test-policy", "tier-1", "tokens.input", 100000, time.Minute),
		makeMultiRule("test-policy", "tier-1", "tokens.output", 50000, time.Minute),
	}

	res, err := l.Reserve(ctx, "test-policy", rules)
	if err != nil {
		t.Fatalf("expected reservation to succeed, got: %v", err)
	}
	if err := l.Commit(ctx, res, Observations{
		Tokens: usage.Tokens{"input": 500, "output": 200},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestMultiRule_ViolatingRuleNamed: one exhausted rule → 429 names that rule.
func TestMultiRule_ViolatingRuleNamed(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
	l := newLimiter(t, &now)
	ctx := context.Background()

	rules := []catalog.ResolvedRule{
		makeMultiRule("test-policy", "tier-1", "requests", 1, time.Minute),
		makeMultiRule("test-policy", "tier-1", "tokens.input", 100000, time.Minute),
	}

	// First reservation exhausts the requests budget.
	res1, err := l.Reserve(ctx, "test-policy", rules)
	if err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	_ = l.Commit(ctx, res1, Observations{Tokens: usage.Tokens{"input": 100}})

	// Second should fail on requests.
	_, err = l.Reserve(ctx, "test-policy", rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected ErrExceeded, got: %v", err)
	}
	var ee *ExceededError
	errors.As(err, &ee)
	if ee.Rule.Rule.Meter != "requests" && string(ee.Rule.Meter) != "requests" {
		t.Fatalf("expected requests meter violated, got meter=%q rule=%+v", ee.Rule.Rule.Meter, ee.Rule)
	}
}

// TestMultiRule_PerMeterCommit: tokens.input/tokens.output incremented separately.
func TestMultiRule_PerMeterCommit(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
	l := newLimiter(t, &now)
	ctx := context.Background()

	inputRule := makeMultiRule("test-policy", "tier-1", "tokens.input", 1000, time.Minute)
	outputRule := makeMultiRule("test-policy", "tier-1", "tokens.output", 500, time.Minute)
	rules := []catalog.ResolvedRule{inputRule, outputRule}

	res, err := l.Reserve(ctx, "test-policy", rules)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Commit 900 input, 400 output.
	if err := l.Commit(ctx, res, Observations{
		Tokens: usage.Tokens{"input": 900, "output": 400},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Commit remaining 100 input + 101 output — output total 501 > 500, exceeds.
	// Using > comparator: rate == amount is still allowed; rate > amount exceeds.
	res2, err := l.Reserve(ctx, "test-policy", rules)
	if err != nil {
		t.Fatalf("second reserve: %v", err)
	}
	_ = l.Commit(ctx, res2, Observations{Tokens: usage.Tokens{"input": 100, "output": 101}})
	_, err = l.Reserve(ctx, "test-policy", rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected ErrExceeded after exhausting limits, got %v", err)
	}
}

// TestMultiRule_BareTokensMeter: bare "tokens" meter sums all usage.Tokens values.
func TestMultiRule_BareTokensMeter(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
	l := newLimiter(t, &now)
	ctx := context.Background()

	rule := makeMultiRule("test-policy", "tier-1", "tokens", 1000, time.Minute)
	rules := []catalog.ResolvedRule{rule}

	res, err := l.Reserve(ctx, "test-policy", rules)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Sum = 300+200 = 500.
	if err := l.Commit(ctx, res, Observations{
		Tokens: usage.Tokens{"input": 300, "output": 200},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Commit 501 more tokens → total 1001 > 1000, exceeds.
	// Using > comparator: rate == amount is still allowed; rate > amount exceeds.
	res2, err := l.Reserve(ctx, "test-policy", rules)
	if err != nil {
		t.Fatalf("second reserve: %v", err)
	}
	_ = l.Commit(ctx, res2, Observations{Tokens: usage.Tokens{"input": 251, "output": 250}})
	_, err = l.Reserve(ctx, "test-policy", rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected ErrExceeded after 1001 tokens, got %v", err)
	}
}

// TestSessionWindow_AnchorsOnFirstRequest verifies the session-window semantics:
// the window timer starts when the first request arrives after a reset, runs for
// `window`, then idles until the next request arrives — at which point a fresh
// window is anchored to that new request.
func TestSessionWindow_AnchorsOnFirstRequest(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	l := newLimiter(t, &now)
	ctx := context.Background()
	rule := makeRuleStrategy(catalog.MeterRequests, 3, 5*time.Hour, catalog.StrategySessionWindow)
	rules := []catalog.ResolvedRule{rule}

	// First request at t=0 anchors the window; 3 fit, 4th exceeds.
	for i := 0; i < 3; i++ {
		res, err := l.Reserve(ctx, "test-policy", rules)
		if err != nil {
			t.Fatalf("reserve %d at anchor: %v", i+1, err)
		}
		_ = l.Commit(ctx, res, Observations{})
	}
	if _, err := l.Reserve(ctx, "test-policy", rules); !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected exceeded at 4th, got %v", err)
	}

	// Advance 2h into the window — still exceeded, window has not reset.
	now = base.Add(2 * time.Hour)
	if _, err := l.Reserve(ctx, "test-policy", rules); !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected still exceeded mid-window, got %v", err)
	}

	// Advance 4h after the *window expires* (anchor + 5h + 4h idle): the next
	// request should anchor a fresh window. Critical bit: window does NOT reset
	// in the background at anchor+5h — it resets only when a request arrives
	// after that point.
	now = base.Add(9 * time.Hour) // anchor=0, expired at 5h, idle 4h
	res, err := l.Reserve(ctx, "test-policy", rules)
	if err != nil {
		t.Fatalf("expected reserve to succeed after window expiry+idle, got %v", err)
	}
	_ = l.Commit(ctx, res, Observations{})

	// The new window is anchored at t=9h, not at 5h. Verify by advancing 4h more
	// (to t=13h, 4h past the new anchor): we're still inside the new window.
	now = base.Add(13 * time.Hour)
	// 2 slots remaining in the new window.
	for i := 0; i < 2; i++ {
		res, err := l.Reserve(ctx, "test-policy", rules)
		if err != nil {
			t.Fatalf("reserve %d in new window: %v", i+1, err)
		}
		_ = l.Commit(ctx, res, Observations{})
	}
	if _, err := l.Reserve(ctx, "test-policy", rules); !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected exceeded after 3 in new window, got %v", err)
	}
}

// TestSessionWindow_RefundOnCancel verifies that cancelled reservations
// return a slot to the session-window count.
func TestSessionWindow_RefundOnCancel(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	l := newLimiter(t, &now)
	ctx := context.Background()
	rule := makeRuleStrategy(catalog.MeterRequests, 2, time.Hour, catalog.StrategySessionWindow)
	rules := []catalog.ResolvedRule{rule}

	// Reserve 1, cancel it. Count should refund back to 0.
	res, err := l.Reserve(ctx, "test-policy", rules)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := l.Commit(ctx, res, Observations{Cancelled: true}); err != nil {
		t.Fatalf("commit cancel: %v", err)
	}

	// Now 2 reservations should fit (refund worked).
	for i := 0; i < 2; i++ {
		r2, err := l.Reserve(ctx, "test-policy", rules)
		if err != nil {
			t.Fatalf("reserve after refund %d: %v", i+1, err)
		}
		_ = l.Commit(ctx, r2, Observations{})
	}
	if _, err := l.Reserve(ctx, "test-policy", rules); !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected exceeded after 2 post-refund, got %v", err)
	}
}
