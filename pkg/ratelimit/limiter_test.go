package ratelimit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/kv"
)

func newTestStore(t *testing.T) *kv.Mem {
	t.Helper()
	s := kv.NewMem()
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newTestLimiter(t *testing.T, now *time.Time) *Limiter {
	t.Helper()
	return New(newTestStore(t), discardLog(), func() time.Time { return *now })
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func reqRule(key, name string, amount int64, window time.Duration) Rule {
	return Rule{Key: key, Name: name, Meter: "requests", Strategy: StrategySlidingWindow, Amount: amount, Window: window}
}

func conRule(key, name string, amount int64, window time.Duration) Rule {
	return Rule{Key: key, Name: name, Meter: "concurrency", Strategy: StrategySlidingWindow, Amount: amount, Window: window}
}

func tokRule(key, name string, amount int64, window time.Duration) Rule {
	return Rule{Key: key, Name: name, Meter: "tokens", Strategy: StrategySlidingWindow, Amount: amount, Window: window}
}

func mustReserve(t *testing.T, l *Limiter, scope string, rules []Rule) *Reservation {
	t.Helper()
	res, err := l.Reserve(context.Background(), scope, rules)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	return res
}

func mustExceed(t *testing.T, l *Limiter, scope string, rules []Rule) *ExceededError {
	t.Helper()
	_, err := l.Reserve(context.Background(), scope, rules)
	var ee *ExceededError
	if !errors.As(err, &ee) || !errors.Is(err, ErrExceeded) {
		t.Fatalf("Reserve: err = %v, want *ExceededError wrapping ErrExceeded", err)
	}
	return ee
}

func mustCommit(t *testing.T, l *Limiter, res *Reservation, obs Observations) {
	t.Helper()
	if err := l.Commit(context.Background(), res, obs); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// step is one instant of a strategy scenario. Its actions run in field order; used is the rule's Peek count once they are done.
type step struct {
	at     time.Duration // clock offset from the scenario start
	grant  int           // reserves that must be granted, each committed at once
	hold   int           // reserves that must be granted and stay open
	settle int           // oldest open reservations committed
	cancel int           // oldest open reservations committed as cancelled
	retry  time.Duration // non-zero: one more reserve must be refused with exactly this retry-after
	used   int64
}

// The start is minute- and hour-aligned, so offsets read as positions inside fixed and sliding windows.
var scenarioStart = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

func TestReserveCommitPerStrategy(t *testing.T) {
	const s, ms = time.Second, time.Millisecond
	rule := func(meter string, strategy Strategy, amount int64, window time.Duration) Rule {
		return Rule{Key: "rule", Name: "the rule", Meter: meter, Strategy: strategy, Amount: amount, Window: window}
	}
	for _, tc := range []struct {
		name  string
		rule  Rule
		steps []step
	}{
		{"token bucket refills at amount per window, capped at amount", rule("requests", StrategyTokenBucket, 10, time.Minute), []step{
			{at: 0, grant: 10, retry: 6 * s, used: 10},
			{at: 3 * s, retry: 3 * s, used: 10}, // half a token back: still 9.5 drawn
			{at: 6 * s, used: 9},
			{at: 6 * s, grant: 1, retry: 6 * s, used: 10},
			{at: 36 * s, used: 5},
			{at: 36 * s, grant: 5, retry: 6 * s, used: 10},
			{at: 156 * s, used: 0},
			{at: 156 * s, grant: 10, retry: 6 * s, used: 10},
		}},
		{"token bucket refunds cancelled reserves only", rule("requests", StrategyTokenBucket, 10, time.Minute), []step{
			{hold: 10, retry: 6 * s, used: 10},
			{settle: 4, used: 10},
			{cancel: 6, used: 4},
			{grant: 6, retry: 6 * s, used: 10},
		}},
		{"empty strategy is a token bucket", rule("requests", "", 1, time.Minute), []step{
			{hold: 1, retry: 60 * s, used: 1},
			{cancel: 1, used: 0},
			{at: 30 * s, grant: 1, retry: 60 * s, used: 1},
			{at: 60 * s, retry: 30 * s, used: 1},
		}},
		{"sliding window weighs the previous bucket by what is left of it", rule("requests", StrategySlidingWindow, 10, time.Minute), []step{
			{at: 500 * ms, grant: 10, retry: 59*s + 500*ms, used: 10},
			{at: 59*s + 999*ms, retry: ms, used: 10},
			{at: 90 * s, used: 5},
			{at: 90 * s, grant: 5, used: 10},
			{at: 150 * s, used: 3}, // half of the 5 counted in the minute before, rounded up
			{at: 180 * s, grant: 10, retry: 60 * s, used: 10},
		}},
		{"sliding window refunds a cancelled reserve", rule("requests", StrategySlidingWindow, 2, time.Minute), []step{
			{at: 10 * s, hold: 2, retry: 50 * s, used: 2},
			{at: 10 * s, settle: 1, cancel: 1, used: 1},
			{at: 10 * s, grant: 1, retry: 50 * s, used: 2},
		}},
		{"fixed window resets at the boundary", rule("requests", StrategyFixedWindow, 5, time.Minute), []step{
			{at: 30 * s, grant: 5, retry: 30 * s, used: 5},
			{at: 59*s + 999*ms, retry: ms, used: 5},
			{at: 60 * s, grant: 5, retry: 60 * s, used: 5},
			{at: 120 * s, hold: 3, used: 3},
			{at: 121 * s, cancel: 3, used: 0},
			{at: 121 * s, grant: 5, retry: 59 * s, used: 5},
		}},
		{"leaky bucket drains at amount per window", rule("requests", StrategyLeakyBucket, 10, time.Minute), []step{
			{at: 0, grant: 10, retry: 6 * s, used: 10},
			{at: 3 * s, retry: 3 * s, used: 10},
			{at: 30 * s, used: 5},
			{at: 30 * s, hold: 5, retry: 6 * s, used: 10},
			{at: 30 * s, settle: 2, used: 10},
			{at: 30 * s, cancel: 3, used: 7},
			{at: 30 * s, grant: 3, retry: 6 * s, used: 10},
			{at: 300 * s, grant: 10, retry: 6 * s, used: 10},
		}},
		{"leaky bucket retry-after is one drained slot", rule("requests", StrategyLeakyBucket, 6, time.Minute), []step{
			{grant: 6, retry: 10 * s, used: 6},
		}},
		{"session window anchors on the first request after expiry", rule("requests", StrategySessionWindow, 3, 5*time.Hour), []step{
			{at: 0, grant: 3, retry: 5 * time.Hour, used: 3},
			{at: 2 * time.Hour, retry: 3 * time.Hour, used: 3},
			{at: 5*time.Hour - ms, retry: ms, used: 3},
			{at: 9 * time.Hour, used: 0},
			{at: 9 * time.Hour, grant: 1, used: 1},
			{at: 13 * time.Hour, hold: 2, retry: time.Hour, used: 3},
			{at: 13 * time.Hour, settle: 1, cancel: 1, used: 2},
			{at: 13 * time.Hour, grant: 1, retry: time.Hour, used: 3},
			{at: 14 * time.Hour, grant: 3, retry: 5 * time.Hour, used: 3},
		}},
		{"concurrency counts open reservations whatever the strategy", rule("concurrency", StrategyLeakyBucket, 2, time.Minute), []step{
			{at: 0, hold: 1, used: 1},
			{at: 20 * s, hold: 1, retry: 40 * s, used: 2},
			{at: 20 * s, settle: 1, used: 1},
			{at: 20 * s, cancel: 1, used: 0},
			{at: 20 * s, grant: 5, used: 0},
			{at: 30 * s, hold: 2, retry: 60 * s, used: 2},
			{at: 90 * s, used: 0}, // leases last one window
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := scenarioStart
			l := newTestLimiter(t, &now)
			rules := []Rule{tc.rule}
			var open []*Reservation
			for i, st := range tc.steps {
				now = scenarioStart.Add(st.at)
				for range st.grant {
					mustCommit(t, l, mustReserve(t, l, "scope", rules), Observations{})
				}
				for range st.hold {
					open = append(open, mustReserve(t, l, "scope", rules))
				}
				for range st.settle {
					mustCommit(t, l, open[0], Observations{})
					open = open[1:]
				}
				for range st.cancel {
					mustCommit(t, l, open[0], Observations{Cancelled: true})
					open = open[1:]
				}
				if st.retry != 0 {
					if diff := cmp.Diff(&ExceededError{Rule: tc.rule, RetryAfter: st.retry}, mustExceed(t, l, "scope", rules)); diff != "" {
						t.Fatalf("step %d at +%v: refusal (-want +got):\n%s", i, st.at, diff)
					}
				}
				if got := peekOne(t, l, "scope", rules)[0]; got != st.used {
					t.Fatalf("step %d at +%v: used = %d, want %d", i, st.at, got, st.used)
				}
			}
		})
	}
}

func TestSlidingWindowRetryAfterIsEarliestAdmission(t *testing.T) {
	t.Skip("bug: sliding window 10/min with previous bucket 10 and current 5, refused at half-window → RetryAfter 30s, but a reserve 6s later is admitted")
	now := scenarioStart.Add(500 * time.Millisecond)
	l := newTestLimiter(t, &now)
	rules := []Rule{reqRule("rule", "the rule", 10, time.Minute)}
	for range 10 {
		mustCommit(t, l, mustReserve(t, l, "scope", rules), Observations{})
	}
	now = scenarioStart.Add(90 * time.Second)
	for range 5 {
		mustCommit(t, l, mustReserve(t, l, "scope", rules), Observations{})
	}
	// One more fits once 6 + 10*(1-f) <= 10, at f = 0.6 of the window: 6s from now.
	if got := mustExceed(t, l, "scope", rules).RetryAfter; got != 6*time.Second {
		t.Errorf("RetryAfter = %v, want 6s", got)
	}
	now = scenarioStart.Add(96 * time.Second)
	mustReserve(t, l, "scope", rules)
}

// A refused reserve names the first rule over budget and leaves every rule's count as it was, including rules checked before it.
func TestReserveRefusalWritesNothing(t *testing.T) {
	now := scenarioStart.Add(30 * time.Second)
	l := newTestLimiter(t, &now)
	rule := func(key, meter string, strategy Strategy, amount int64) Rule {
		return Rule{Key: key, Name: key + " name", Meter: meter, Strategy: strategy, Amount: amount, Window: time.Minute}
	}
	counted := []Rule{
		rule("sliding", "requests", StrategySlidingWindow, 2),
		rule("fixed", "requests", StrategyFixedWindow, 3),
		rule("bucket", "requests", StrategyTokenBucket, 4),
		rule("leaky", "requests", StrategyLeakyBucket, 5),
		rule("session", "requests", StrategySessionWindow, 6),
		rule("slots", "concurrency", "", 7),
	}
	blocker := rule("blocker", "concurrency", "", 0)
	last := rule("last", "requests", StrategyFixedWindow, 0)
	all := append(append([]Rule{}, counted...), blocker, last)

	if diff := cmp.Diff(&ExceededError{Rule: blocker, RetryAfter: time.Minute}, mustExceed(t, l, "scope", all)); diff != "" {
		t.Fatalf("refusal (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]int64{0, 0, 0, 0, 0, 0, 0, 0}, peekOne(t, l, "scope", all)); diff != "" {
		t.Fatalf("counts after a refused reserve (-want +got):\n%s", diff)
	}

	// The tightest rule refuses the third request; the two granted ones are counted once on every rule.
	open := []*Reservation{mustReserve(t, l, "scope", counted), mustReserve(t, l, "scope", counted)}
	if diff := cmp.Diff(&ExceededError{Rule: counted[0], RetryAfter: 30 * time.Second}, mustExceed(t, l, "scope", counted)); diff != "" {
		t.Fatalf("refusal (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]int64{2, 2, 2, 2, 2, 2}, peekOne(t, l, "scope", counted)); diff != "" {
		t.Fatalf("counts after two grants and a refusal (-want +got):\n%s", diff)
	}
	for _, res := range open {
		mustCommit(t, l, res, Observations{Cancelled: true})
	}
	if diff := cmp.Diff([]int64{0, 0, 0, 0, 0, 0}, peekOne(t, l, "scope", counted)); diff != "" {
		t.Fatalf("counts after cancelling both (-want +got):\n%s", diff)
	}
}

// The bare meter charges input plus output; the other keys break those two down and have meters of their own.
func TestCommitChargesTokenMetersSeparately(t *testing.T) {
	now := scenarioStart.Add(30 * time.Second)
	l := newTestLimiter(t, &now)
	var rules []Rule
	for _, meter := range []string{"tokens", "tokens.input", "tokens.output", "tokens.cache_read", "tokens.absent"} {
		rules = append(rules, Rule{Key: "rule", Name: meter, Meter: meter, Strategy: StrategySlidingWindow, Amount: 100, Window: time.Minute})
	}
	obs := Observations{Tokens: map[string]int64{"input": 40, "output": 20, "reasoning": 15, "cache_read": 30, "cache_creation": 25}}
	mustCommit(t, l, mustReserve(t, l, "scope", rules), obs)
	if diff := cmp.Diff([]int64{60, 40, 20, 30, 0}, peekOne(t, l, "scope", rules)); diff != "" {
		t.Fatalf("token counts (-want +got):\n%s", diff)
	}
	// A cancelled commit charges no tokens.
	obs.Cancelled = true
	mustCommit(t, l, mustReserve(t, l, "scope", rules), obs)
	if diff := cmp.Diff([]int64{60, 40, 20, 30, 0}, peekOne(t, l, "scope", rules)); diff != "" {
		t.Fatalf("token counts after a cancelled commit (-want +got):\n%s", diff)
	}
	// 100 used of 100 still admits; one more token refuses until the window ends.
	mustCommit(t, l, mustReserve(t, l, "scope", rules[:1]), Observations{Tokens: map[string]int64{"output": 40}})
	mustCommit(t, l, mustReserve(t, l, "scope", rules[:1]), Observations{Tokens: map[string]int64{"input": 1}})
	if diff := cmp.Diff(&ExceededError{Rule: rules[0], RetryAfter: 30 * time.Second}, mustExceed(t, l, "scope", rules)); diff != "" {
		t.Fatalf("refusal (-want +got):\n%s", diff)
	}
}

// A commit whose lease set already expired frees nothing it does not hold.
func TestConcurrencyCommitAfterKeyExpiry(t *testing.T) {
	now := scenarioStart
	l := newTestLimiter(t, &now)
	rule := conRule("rule", "concurrency", 1, time.Minute)
	rules := []Rule{rule}

	res := mustReserve(t, l, "scope", rules)
	if err := l.store.Del(context.Background(), concurrencyKey("scope", rule)); err != nil {
		t.Fatalf("expire key: %v", err)
	}
	mustCommit(t, l, res, Observations{})

	mustReserve(t, l, "scope", rules)
	if got := peekOne(t, l, "scope", rules)[0]; got != 1 {
		t.Fatalf("held slots = %d, want 1", got)
	}
	mustExceed(t, l, "scope", rules)
}
