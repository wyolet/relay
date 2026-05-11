package ratelimit

// store_contract_test.go — parameterised contract suite that runs against
// kv.Mem unconditionally. The Redis variant lives in distributed_test.go
// and is gated on the "integration" build tag.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

func memLimiterFactory(t *testing.T, now *time.Time) *Limiter {
	s := kv.NewMem()
	t.Cleanup(func() { _ = s.Close() })
	clock := func() time.Time { return *now }
	return New(s, discardLog(), clock)
}

func runLimiterContractSuite(t *testing.T, name string, factory func(t *testing.T, now *time.Time) *Limiter) {
	t.Run(name+"/Requests_HappyPath", func(t *testing.T) {
		now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
		l := factory(t, &now)
		ctx := context.Background()
		rule := Rule{
			Key:      "Route:contract-route:rl-req-happy",
			Name:     "requests",
			Meter:    "requests",
			Strategy: StrategySlidingWindow,
			Amount:   10,
			Window:   time.Minute,
		}
		rules := []Rule{rule}
		for i := 0; i < 10; i++ {
			res, err := l.Reserve(ctx, "test-policy", rules)
			if err != nil {
				t.Fatalf("reserve %d: %v", i+1, err)
			}
			_ = l.Commit(ctx, res, Observations{})
		}
		_, err := l.Reserve(ctx, "test-policy", rules)
		if !errors.Is(err, ErrExceeded) {
			t.Fatalf("expected ErrExceeded on 11th reserve, got %v", err)
		}
	})

	t.Run(name+"/Concurrency_BudgetCap", func(t *testing.T) {
		now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		l := factory(t, &now)
		ctx := context.Background()
		rule := Rule{
			Key:      "Route:contract-route:rl-con-cap",
			Name:     "concurrency",
			Meter:    "concurrency",
			Strategy: StrategySlidingWindow,
			Amount:   3,
			Window:   time.Minute,
		}
		rules := []Rule{rule}
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
		if err := l.Commit(ctx, r[0], Observations{}); err != nil {
			t.Fatalf("commit: %v", err)
		}
		res, err := l.Reserve(ctx, "test-policy", rules)
		if err != nil {
			t.Fatalf("reserve after commit: %v", err)
		}
		_ = l.Commit(ctx, res, Observations{})
	})

	t.Run(name+"/Tokens_PostHoc", func(t *testing.T) {
		now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
		l := factory(t, &now)
		ctx := context.Background()
		rule := Rule{
			Key:      "Route:contract-route:rl-tok-posthoc",
			Name:     "tokens",
			Meter:    "tokens",
			Strategy: StrategySlidingWindow,
			Amount:   100,
			Window:   time.Minute,
		}
		rules := []Rule{rule}
		var reservations [5]*Reservation
		for i := 0; i < 5; i++ {
			res, err := l.Reserve(ctx, "test-policy", rules)
			if err != nil {
				t.Fatalf("reserve %d: %v", i+1, err)
			}
			reservations[i] = res
		}
		for i, res := range reservations {
			if err := l.Commit(ctx, res, Observations{Tokens: map[string]int64{"tokens": 20}}); err != nil {
				t.Fatalf("commit %d: %v", i+1, err)
			}
		}
		_, err := l.Reserve(ctx, "test-policy", rules)
		if !errors.Is(err, ErrExceeded) {
			t.Fatalf("expected ErrExceeded after 100 tokens, got %v", err)
		}
	})

	t.Run(name+"/IdempotentCommit", func(t *testing.T) {
		now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		l := factory(t, &now)
		ctx := context.Background()
		rule := Rule{
			Key:      "Route:contract-route:rl-idem-commit",
			Name:     "concurrency",
			Meter:    "concurrency",
			Strategy: StrategySlidingWindow,
			Amount:   1,
			Window:   time.Minute,
		}
		rules := []Rule{rule}
		res, err := l.Reserve(ctx, "test-policy", rules)
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		if err := l.Commit(ctx, res, Observations{Tokens: map[string]int64{"tokens": 50}}); err != nil {
			t.Fatalf("commit 1: %v", err)
		}
		if err := l.Commit(ctx, res, Observations{Tokens: map[string]int64{"tokens": 50}}); err != nil {
			t.Fatalf("commit 2: %v", err)
		}
		rem, err := l.RemainingByMeter(ctx, "test-policy", rules)
		if err != nil {
			t.Fatalf("remaining: %v", err)
		}
		if rem["concurrency"] != 1 {
			t.Fatalf("expected concurrency remaining=1 after single commit, got %d", rem["concurrency"])
		}
	})

	t.Run(name+"/Rollback_FirstViolation", func(t *testing.T) {
		now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
		l := factory(t, &now)
		ctx := context.Background()
		rule0 := Rule{
			Key:      "Route:contract-route:rl-rule0",
			Name:     "requests on rl-rule0",
			Meter:    "requests",
			Strategy: StrategySlidingWindow,
			Amount:   100,
			Window:   time.Minute,
		}
		rule1 := Rule{
			Key:      "Route:contract-route:rl-rule1",
			Name:     "concurrency on rl-rule1",
			Meter:    "concurrency",
			Strategy: StrategySlidingWindow,
			Amount:   0, // cap=0, always fails
			Window:   time.Minute,
		}
		rules := []Rule{rule0, rule1}

		_, err := l.Reserve(ctx, "test-policy", rules)
		if !errors.Is(err, ErrExceeded) {
			t.Fatalf("expected exceeded, got %v", err)
		}
		var ee *ExceededError
		errors.As(err, &ee)
		if ee.Rule.Key != rule1.Key {
			t.Fatalf("expected rule1 to be violated (key=%s), got key=%s", rule1.Key, ee.Rule.Key)
		}
		rem, err := l.RemainingByMeter(ctx, "test-policy", []Rule{rule0})
		if err != nil {
			t.Fatalf("remaining: %v", err)
		}
		if rem["requests"] != 100 {
			t.Fatalf("expected rule0 remaining=100 after rollback, got %d", rem["requests"])
		}
	})
}

func TestContractLimit_MemStore(t *testing.T) {
	runLimiterContractSuite(t, "MemStore", memLimiterFactory)
}
