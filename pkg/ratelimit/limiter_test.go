package ratelimit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func newTestStore(t *testing.T) *kv.Mem {
	t.Helper()
	s := kv.NewMem()
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newTestLimiter(t *testing.T, now *time.Time) *Limiter {
	t.Helper()
	s := newTestStore(t)
	clock := func() time.Time { return *now }
	return New(s, discardLog(), clock)
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// reqRule builds a requests/sliding-window Rule directly.
func reqRule(key, name string, amount int64, window time.Duration) Rule {
	return Rule{
		Key:      key,
		Name:     name,
		Meter:    "requests",
		Strategy: StrategySlidingWindow,
		Amount:   amount,
		Window:   window,
	}
}

func conRule(key, name string, amount int64, window time.Duration) Rule {
	return Rule{
		Key:      key,
		Name:     name,
		Meter:    "concurrency",
		Strategy: StrategySlidingWindow, // ignored for concurrency
		Amount:   amount,
		Window:   window,
	}
}

func tokRule(key, name string, amount int64, window time.Duration) Rule {
	return Rule{
		Key:      key,
		Name:     name,
		Meter:    "tokens",
		Strategy: StrategySlidingWindow,
		Amount:   amount,
		Window:   window,
	}
}

const testScope = "test-policy"

// ── migrated tests ────────────────────────────────────────────────────────────

// TestRequests_RPMWindow_HappyPath: amount=10, 10 requests succeed, 11th fails.
func TestRequests_RPMWindow_HappyPath(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
	l := newTestLimiter(t, &now)
	ctx := context.Background()
	rules := []Rule{reqRule("Route:test-route:rl-requests", "requests on rl-requests", 10, time.Minute)}

	for i := 0; i < 10; i++ {
		res, err := l.Reserve(ctx, testScope, rules)
		if err != nil {
			t.Fatalf("reserve %d: unexpected error: %v", i+1, err)
		}
		_ = l.Commit(ctx, res, Observations{})
	}

	_, err := l.Reserve(ctx, testScope, rules)
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
	if ee.Rule.Meter != "requests" {
		t.Fatalf("expected requests meter, got %s", ee.Rule.Meter)
	}
}

// TestRequests_SlidingInterpolation: at half-window, old bucket weight = 0.5.
func TestRequests_SlidingInterpolation(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base.Add(500 * time.Millisecond)
	l := newTestLimiter(t, &now)
	ctx := context.Background()
	rules := []Rule{reqRule("Route:test-route:rl-requests", "requests", 10, time.Minute)}

	// Fill 10 requests in the first bucket.
	for i := 0; i < 10; i++ {
		res, err := l.Reserve(ctx, testScope, rules)
		if err != nil {
			t.Fatalf("fill reserve %d: %v", i+1, err)
		}
		_ = l.Commit(ctx, res, Observations{})
	}

	// Advance to t=1min+30s: we're 30s into the second window.
	// Previous bucket (10 reqs) has weight 0.5 → up to ~5 more allowed.
	now = base.Add(time.Minute + 30*time.Second)
	okCount := 0
	for i := 0; i < 10; i++ {
		res, err := l.Reserve(ctx, testScope, rules)
		if err != nil {
			if !errors.Is(err, ErrExceeded) {
				t.Fatalf("unexpected error: %v", err)
			}
			break
		}
		okCount++
		_ = l.Commit(ctx, res, Observations{})
	}
	if okCount < 4 || okCount > 6 {
		t.Fatalf("expected ~5 requests at half-window, got %d", okCount)
	}

	// At t=3min: all prior buckets expired (2*window TTL). Fresh 10 slots.
	now = base.Add(3 * time.Minute)
	for i := 0; i < 10; i++ {
		res, err := l.Reserve(ctx, testScope, rules)
		if err != nil {
			t.Fatalf("new window reserve %d: %v", i+1, err)
		}
		_ = l.Commit(ctx, res, Observations{})
	}
	_, err := l.Reserve(ctx, testScope, rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected exceeded after 10 in new window, got %v", err)
	}
}

// TestTokens_PostHocOnly: tokens peeked at Reserve, incremented at Commit.
func TestTokens_PostHocOnly(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
	l := newTestLimiter(t, &now)
	ctx := context.Background()
	rules := []Rule{tokRule("Route:test-route:rl-tokens", "tokens", 100, time.Minute)}

	// 5 reserves succeed (tokens not yet consumed at Reserve time).
	var reservations [5]*Reservation
	for i := 0; i < 5; i++ {
		res, err := l.Reserve(ctx, testScope, rules)
		if err != nil {
			t.Fatalf("reserve %d: %v", i+1, err)
		}
		reservations[i] = res
	}

	// Commit each with 20 tokens → total 100.
	for i, res := range reservations {
		if err := l.Commit(ctx, res, Observations{Tokens: map[string]int64{"tokens": 20}}); err != nil {
			t.Fatalf("commit %d: %v", i+1, err)
		}
	}

	// 6th Reserve should fail: tokens=100 >= 100.
	_, err := l.Reserve(ctx, testScope, rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected ErrExceeded after 100 tokens consumed, got %v", err)
	}
	var ee *ExceededError
	if !errors.As(err, &ee) || ee.Rule.Meter != "tokens" {
		t.Fatalf("expected tokens meter exceeded, got %v", err)
	}
}

// TestConcurrency_BudgetCap: amount=3, 3 succeed, 4th fails, after commit 5th succeeds.
func TestConcurrency_BudgetCap(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newTestLimiter(t, &now)
	ctx := context.Background()
	rules := []Rule{conRule("Route:test-route:rl-concurrency", "concurrency", 3, time.Minute)}

	var r [3]*Reservation
	for i := 0; i < 3; i++ {
		res, err := l.Reserve(ctx, testScope, rules)
		if err != nil {
			t.Fatalf("reserve %d: %v", i+1, err)
		}
		r[i] = res
	}

	_, err := l.Reserve(ctx, testScope, rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected ErrExceeded on 4th, got %v", err)
	}

	if err := l.Commit(ctx, r[0], Observations{}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	res, err := l.Reserve(ctx, testScope, rules)
	if err != nil {
		t.Fatalf("reserve after commit: %v", err)
	}
	_ = l.Commit(ctx, res, Observations{})
}

// TestConcurrency_CommitOnCancel_DecreasesCounter
func TestConcurrency_CommitOnCancel_DecreasesCounter(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newTestLimiter(t, &now)
	ctx := context.Background()
	rules := []Rule{conRule("Route:test-route:rl-concurrency", "concurrency", 1, time.Minute)}

	res, err := l.Reserve(ctx, testScope, rules)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}

	_, err = l.Reserve(ctx, testScope, rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected exceeded, got %v", err)
	}

	if err := l.Commit(ctx, res, Observations{Cancelled: true}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	res2, err := l.Reserve(ctx, testScope, rules)
	if err != nil {
		t.Fatalf("reserve after cancel commit: %v", err)
	}
	_ = l.Commit(ctx, res2, Observations{})
}

// TestComposition_FirstViolationShortCircuits
func TestComposition_FirstViolationShortCircuits(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
	l := newTestLimiter(t, &now)
	ctx := context.Background()

	rule0 := reqRule("Route:test-route:rl-rule0", "requests on rl-rule0", 100, time.Minute)
	rule1 := conRule("Route:test-route:rl-rule1", "concurrency on rl-rule1", 0, time.Minute) // cap=0, always fails
	rule2 := reqRule("Route:test-route:rl-rule2", "requests on rl-rule2", 100, time.Minute)

	rules := []Rule{rule0, rule1, rule2}

	_, err := l.Reserve(ctx, testScope, rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected exceeded, got %v", err)
	}

	var ee *ExceededError
	errors.As(err, &ee)
	if ee.Rule.Key != rule1.Key {
		t.Fatalf("expected rule1 to be violated (key=%s), got key=%s", rule1.Key, ee.Rule.Key)
	}

	// rule0's requests counter should be rolled back → 100 consecutive reserves succeed.
	for i := 0; i < 100; i++ {
		res, err2 := l.Reserve(ctx, testScope, []Rule{rule0})
		if err2 != nil {
			t.Fatalf("rule0 reserve %d after rollback: %v", i+1, err2)
		}
		_ = l.Commit(ctx, res, Observations{})
	}
}

// TestIdempotentCommit: double Commit is a no-op.
func TestIdempotentCommit(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newTestLimiter(t, &now)
	ctx := context.Background()
	rules := []Rule{conRule("Route:test-route:rl-concurrency", "concurrency", 1, time.Minute)}

	res, err := l.Reserve(ctx, testScope, rules)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}

	obs := Observations{Tokens: map[string]int64{"tokens": 50}}
	if err := l.Commit(ctx, res, obs); err != nil {
		t.Fatalf("commit 1: %v", err)
	}
	if err := l.Commit(ctx, res, obs); err != nil {
		t.Fatalf("commit 2: %v", err)
	}

	// Concurrency counter should be 0 (decremented once, not twice → a third reserve succeeds).
	res3, err := l.Reserve(ctx, testScope, rules)
	if err != nil {
		t.Fatalf("expected third reserve to succeed after idempotent commit, got %v", err)
	}
	_ = l.Commit(ctx, res3, Observations{})
}

// TestSlidingWindow_BoundaryAccuracy
func TestSlidingWindow_BoundaryAccuracy(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 1, 0, 0, time.UTC) // minute boundary
	rule := reqRule("Route:test-route:rl-requests", "requests", 5, time.Minute)

	// Fill 5 in bucket starting at base.
	{
		now := base.Add(500 * time.Millisecond)
		l := newTestLimiter(t, &now)
		ctx := context.Background()
		for i := 0; i < 5; i++ {
			res, err := l.Reserve(ctx, testScope, []Rule{rule})
			if err != nil {
				t.Fatalf("fill %d: %v", i+1, err)
			}
			_ = l.Commit(ctx, res, Observations{})
		}
		_, err := l.Reserve(ctx, testScope, []Rule{rule})
		if !errors.Is(err, ErrExceeded) {
			t.Fatalf("expected exceeded at t=0.5s, got %v", err)
		}
	}

	// At t=59.999s: still exceeded.
	{
		now := base.Add(59*time.Second + 999*time.Millisecond)
		l := newTestLimiter(t, &now)
		ctx := context.Background()
		for i := 0; i < 5; i++ {
			res, err := l.Reserve(ctx, testScope, []Rule{rule})
			if err != nil {
				t.Fatalf("fill at 59.999s %d: %v", i+1, err)
			}
			_ = l.Commit(ctx, res, Observations{})
		}
		_, err := l.Reserve(ctx, testScope, []Rule{rule})
		if !errors.Is(err, ErrExceeded) {
			t.Fatalf("expected exceeded at t=59.999s, got %v", err)
		}
	}

	// At t=60.001s (next bucket): resets.
	{
		now := base.Add(60*time.Second + time.Millisecond)
		l := newTestLimiter(t, &now)
		ctx := context.Background()
		res, err := l.Reserve(ctx, testScope, []Rule{rule})
		if err != nil {
			t.Fatalf("expected success in new bucket, got %v", err)
		}
		_ = l.Commit(ctx, res, Observations{})
	}
}


// TestReserve_ContextCancel
func TestReserve_ContextCancel(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newTestLimiter(t, &now)
	rules := []Rule{conRule("Route:test-route:rl-concurrency", "concurrency", 2, time.Minute)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	res1, err := l.Reserve(ctx, testScope, rules)
	if err != nil {
		t.Fatalf("reserve 1: %v", err)
	}
	res2, err := l.Reserve(ctx, testScope, rules)
	if err != nil {
		t.Fatalf("reserve 2: %v", err)
	}

	cancel()

	_ = l.Commit(context.Background(), res1, Observations{})
	_ = l.Commit(context.Background(), res2, Observations{})

	// Both commits released their concurrency slots → two new reserves succeed.
	for i := 0; i < 2; i++ {
		res, err2 := l.Reserve(context.Background(), testScope, rules)
		if err2 != nil {
			t.Fatalf("post-cancel reserve %d: expected success, got %v", i+1, err2)
		}
		_ = l.Commit(context.Background(), res, Observations{})
	}
}

// TestFixedWindow_HappyPath: amount=5, 5 succeed, 6th fails, new window resets.
func TestFixedWindow_HappyPath(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
	l := newTestLimiter(t, &now)
	ctx := context.Background()
	rule := Rule{
		Key:      "Route:test-route:rl-fw",
		Name:     "requests on rl-fw",
		Meter:    "requests",
		Strategy: StrategyFixedWindow,
		Amount:   5,
		Window:   time.Minute,
	}
	rules := []Rule{rule}

	for i := 0; i < 5; i++ {
		res, err := l.Reserve(ctx, testScope, rules)
		if err != nil {
			t.Fatalf("reserve %d: %v", i+1, err)
		}
		_ = l.Commit(ctx, res, Observations{})
	}
	_, err := l.Reserve(ctx, testScope, rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected exceeded on 6th, got %v", err)
	}

	now = now.Add(time.Minute)
	res, err := l.Reserve(ctx, testScope, rules)
	if err != nil {
		t.Fatalf("expected success in new window, got %v", err)
	}
	_ = l.Commit(ctx, res, Observations{})
}

// TestTokenBucket_Refill: burst=5, exhaust, advance 12s (1 token refilled).
func TestTokenBucket_Refill(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newTestLimiter(t, &now)
	ctx := context.Background()
	rule := Rule{
		Key:      "Route:test-route:rl-tb",
		Name:     "requests on rl-tb",
		Meter:    "requests",
		Strategy: StrategyTokenBucket,
		Amount:   5,
		Window:   time.Minute,
	}
	rules := []Rule{rule}

	for i := 0; i < 5; i++ {
		res, err := l.Reserve(ctx, testScope, rules)
		if err != nil {
			t.Fatalf("reserve %d: %v", i+1, err)
		}
		_ = l.Commit(ctx, res, Observations{})
	}
	_, err := l.Reserve(ctx, testScope, rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected exceeded after burst exhausted, got %v", err)
	}

	// Advance 12s → 1 token refilled (5 tokens/60s = 1/12s).
	now = now.Add(12 * time.Second)
	res, err := l.Reserve(ctx, testScope, rules)
	if err != nil {
		t.Fatalf("expected 1 refilled token, got %v", err)
	}
	_ = l.Commit(ctx, res, Observations{})
}

// TestTokenBucket_RefundOnCancel: cancelled reservation returns token.
func TestTokenBucket_RefundOnCancel(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newTestLimiter(t, &now)
	ctx := context.Background()
	rule := Rule{
		Key:      "Route:test-route:rl-tb",
		Name:     "requests on rl-tb",
		Meter:    "requests",
		Strategy: StrategyTokenBucket,
		Amount:   1,
		Window:   time.Minute,
	}
	rules := []Rule{rule}

	res, err := l.Reserve(ctx, testScope, rules)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	_, err = l.Reserve(ctx, testScope, rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected exceeded, got %v", err)
	}

	_ = l.Commit(ctx, res, Observations{Cancelled: true})

	res2, err := l.Reserve(ctx, testScope, rules)
	if err != nil {
		t.Fatalf("expected success after refund, got %v", err)
	}
	_ = l.Commit(ctx, res2, Observations{})
}

// TestLeakyBucket_DrainAndRefund: queue fills, drains over time, and refund works.
func TestLeakyBucket_DrainAndRefund(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newTestLimiter(t, &now)
	ctx := context.Background()
	rule := Rule{
		Key:      "Route:test-route:rl-lb",
		Name:     "requests on rl-lb",
		Meter:    "requests",
		Strategy: StrategyLeakyBucket,
		Amount:   3,
		Window:   time.Minute,
	}
	rules := []Rule{rule}

	for i := 0; i < 3; i++ {
		res, err := l.Reserve(ctx, testScope, rules)
		if err != nil {
			t.Fatalf("reserve %d: %v", i+1, err)
		}
		_ = l.Commit(ctx, res, Observations{})
	}
	_, err := l.Reserve(ctx, testScope, rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected exceeded after queue full, got %v", err)
	}

	// Advance 20s → 1 slot drained (3/60s = 1/20s).
	now = now.Add(20 * time.Second)
	res, err := l.Reserve(ctx, testScope, rules)
	if err != nil {
		t.Fatalf("expected slot after drain, got %v", err)
	}
	_ = l.Commit(ctx, res, Observations{})
}

// TestLeakyBucket_RefundOnCancel
func TestLeakyBucket_RefundOnCancel(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newTestLimiter(t, &now)
	ctx := context.Background()
	rule := Rule{
		Key:      "Route:test-route:rl-lb",
		Name:     "requests on rl-lb",
		Meter:    "requests",
		Strategy: StrategyLeakyBucket,
		Amount:   1,
		Window:   time.Minute,
	}
	rules := []Rule{rule}

	res, err := l.Reserve(ctx, testScope, rules)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	_, err = l.Reserve(ctx, testScope, rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected exceeded, got %v", err)
	}

	_ = l.Commit(ctx, res, Observations{Cancelled: true})

	res2, err := l.Reserve(ctx, testScope, rules)
	if err != nil {
		t.Fatalf("expected success after cancel refund, got %v", err)
	}
	_ = l.Commit(ctx, res2, Observations{})
}

// TestMultiRule_AllGranted: all rules have headroom → reservation succeeds.
func TestMultiRule_AllGranted(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
	l := newTestLimiter(t, &now)
	ctx := context.Background()

	rules := []Rule{
		{Key: "Policy:test-policy:tier-1", Name: "requests on tier-1", Meter: "requests", Strategy: StrategySlidingWindow, Amount: 10, Window: time.Minute},
		{Key: "Policy:test-policy:tier-1", Name: "tokens.input on tier-1", Meter: "tokens.input", Strategy: StrategySlidingWindow, Amount: 100000, Window: time.Minute},
		{Key: "Policy:test-policy:tier-1", Name: "tokens.output on tier-1", Meter: "tokens.output", Strategy: StrategySlidingWindow, Amount: 50000, Window: time.Minute},
	}

	res, err := l.Reserve(ctx, testScope, rules)
	if err != nil {
		t.Fatalf("expected reservation to succeed, got: %v", err)
	}
	if err := l.Commit(ctx, res, Observations{
		Tokens: map[string]int64{"input": 500, "output": 200},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestMultiRule_ViolatingRuleNamed: one exhausted rule → 429 names that rule.
func TestMultiRule_ViolatingRuleNamed(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
	l := newTestLimiter(t, &now)
	ctx := context.Background()

	rules := []Rule{
		{Key: "Policy:test-policy:tier-1", Name: "requests on tier-1", Meter: "requests", Strategy: StrategySlidingWindow, Amount: 1, Window: time.Minute},
		{Key: "Policy:test-policy:tier-1", Name: "tokens.input on tier-1", Meter: "tokens.input", Strategy: StrategySlidingWindow, Amount: 100000, Window: time.Minute},
	}

	res1, err := l.Reserve(ctx, testScope, rules)
	if err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	_ = l.Commit(ctx, res1, Observations{Tokens: map[string]int64{"input": 100}})

	_, err = l.Reserve(ctx, testScope, rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected ErrExceeded, got: %v", err)
	}
	var ee *ExceededError
	errors.As(err, &ee)
	if ee.Rule.Meter != "requests" {
		t.Fatalf("expected requests meter violated, got meter=%q", ee.Rule.Meter)
	}
}

// TestMultiRule_PerMeterCommit: tokens.input/tokens.output incremented separately.
func TestMultiRule_PerMeterCommit(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
	l := newTestLimiter(t, &now)
	ctx := context.Background()

	rules := []Rule{
		{Key: "Policy:test-policy:tier-1", Name: "tokens.input", Meter: "tokens.input", Strategy: StrategySlidingWindow, Amount: 1000, Window: time.Minute},
		{Key: "Policy:test-policy:tier-1", Name: "tokens.output", Meter: "tokens.output", Strategy: StrategySlidingWindow, Amount: 500, Window: time.Minute},
	}

	res, err := l.Reserve(ctx, testScope, rules)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := l.Commit(ctx, res, Observations{
		Tokens: map[string]int64{"input": 900, "output": 400},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// After committing 900 input and 400 output, a second commit of the same amounts
	// should fail (only 100 input and 100 output remaining).
	res2, err := l.Reserve(ctx, testScope, rules)
	if err != nil {
		t.Fatalf("second reserve: %v", err)
	}
	_ = l.Commit(ctx, res2, Observations{
		Tokens: map[string]int64{"input": 100, "output": 100},
	})
	// Third reserve should now exceed both limits.
	_, err = l.Reserve(ctx, testScope, rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected ErrExceeded after limits exhausted, got %v", err)
	}
}

// TestMultiRule_BareTokensMeter: bare "tokens" meter sums all token values.
func TestMultiRule_BareTokensMeter(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
	l := newTestLimiter(t, &now)
	ctx := context.Background()

	rules := []Rule{tokRule("Policy:test-policy:tier-1", "tokens", 1000, time.Minute)}

	res, err := l.Reserve(ctx, testScope, rules)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := l.Commit(ctx, res, Observations{
		Tokens: map[string]int64{"input": 300, "output": 200},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// After 500 tokens consumed, a second commit of 500 should succeed (total 1000).
	res2, err := l.Reserve(ctx, testScope, rules)
	if err != nil {
		t.Fatalf("second reserve: %v", err)
	}
	_ = l.Commit(ctx, res2, Observations{
		Tokens: map[string]int64{"input": 250, "output": 250},
	})
	// Third reserve should now exceed the 1000-token limit.
	_, err = l.Reserve(ctx, testScope, rules)
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected ErrExceeded after 1000 tokens consumed, got %v", err)
	}
}

// TestSessionWindow_AnchorsOnFirstRequest
func TestSessionWindow_AnchorsOnFirstRequest(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	l := newTestLimiter(t, &now)
	ctx := context.Background()
	rule := Rule{
		Key:      "Route:test-route:rl-sw",
		Name:     "requests on rl-sw",
		Meter:    "requests",
		Strategy: StrategySessionWindow,
		Amount:   3,
		Window:   5 * time.Hour,
	}
	rules := []Rule{rule}

	for i := 0; i < 3; i++ {
		res, err := l.Reserve(ctx, testScope, rules)
		if err != nil {
			t.Fatalf("reserve %d at anchor: %v", i+1, err)
		}
		_ = l.Commit(ctx, res, Observations{})
	}
	if _, err := l.Reserve(ctx, testScope, rules); !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected exceeded at 4th, got %v", err)
	}

	// Mid-window: still exceeded.
	now = base.Add(2 * time.Hour)
	if _, err := l.Reserve(ctx, testScope, rules); !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected still exceeded mid-window, got %v", err)
	}

	// After expiry+idle: fresh window anchors at arrival time.
	now = base.Add(9 * time.Hour)
	res, err := l.Reserve(ctx, testScope, rules)
	if err != nil {
		t.Fatalf("expected success after expiry+idle, got %v", err)
	}
	_ = l.Commit(ctx, res, Observations{})

	// New window anchored at t=9h: advance to t=13h (still inside 5h window from 9h).
	now = base.Add(13 * time.Hour)
	for i := 0; i < 2; i++ {
		res, err := l.Reserve(ctx, testScope, rules)
		if err != nil {
			t.Fatalf("reserve %d in new window: %v", i+1, err)
		}
		_ = l.Commit(ctx, res, Observations{})
	}
	if _, err := l.Reserve(ctx, testScope, rules); !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected exceeded after 3 in new window, got %v", err)
	}
}

// TestSessionWindow_RefundOnCancel
func TestSessionWindow_RefundOnCancel(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	l := newTestLimiter(t, &now)
	ctx := context.Background()
	rule := Rule{
		Key:      "Route:test-route:rl-sw",
		Name:     "requests on rl-sw",
		Meter:    "requests",
		Strategy: StrategySessionWindow,
		Amount:   2,
		Window:   time.Hour,
	}
	rules := []Rule{rule}

	res, err := l.Reserve(ctx, testScope, rules)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := l.Commit(ctx, res, Observations{Cancelled: true}); err != nil {
		t.Fatalf("commit cancel: %v", err)
	}

	// 2 reservations should fit after refund.
	for i := 0; i < 2; i++ {
		r2, err := l.Reserve(ctx, testScope, rules)
		if err != nil {
			t.Fatalf("reserve after refund %d: %v", i+1, err)
		}
		_ = l.Commit(ctx, r2, Observations{})
	}
	if _, err := l.Reserve(ctx, testScope, rules); !errors.Is(err, ErrExceeded) {
		t.Fatalf("expected exceeded after 2 post-refund, got %v", err)
	}
}
