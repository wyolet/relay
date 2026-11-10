package lua

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	"github.com/wyolet/relay/pkg/meta"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	"github.com/wyolet/relay/sdk/usage"
)

const usd = 1_000_000_000

var testRateLimit = &ratelimit.RateLimit{Meta: meta.Metadata{ID: "rl1"}}

func rule(meter ratelimit.Meter, strategy ratelimit.Strategy, amount int64) ratelimit.Rule {
	return ratelimit.Rule{Meter: meter, Strategy: strategy, Amount: amount, Window: ratelimit.Window(time.Minute)}
}

func callerCounter(r ratelimit.Rule, subject string) limits.Counter {
	return limits.CallerRule(limits.RungProject, testRateLimit, r, "p1", subject, "team:t1")
}

func upstreamCounter(r ratelimit.Rule, account string) limits.Counter {
	return limits.UpstreamRule(limits.RungAccount, testRateLimit, r, "p2", "account:"+account, "", "account:"+account)
}

func budgetCounter(amountUSD string, period budget.Period, mode budget.Mode) limits.Counter {
	b := &budget.Budget{Meta: meta.Metadata{ID: "b1"}, Spec: budget.Spec{AmountUSD: amountUSD, Period: period, Mode: mode}}
	return limits.BudgetCounter(limits.RungTeam, b, "p1", "team:t1", "team:t1")
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newStore returns a fresh store for one test; a kv.Mem comes with the limiter's script emulators registered, so wrappers around it work.
type newStore func(t testing.TB) kvtest.ScriptStore

func memStore(t testing.TB) kvtest.ScriptStore {
	m := kv.NewMem()
	t.Cleanup(func() { _ = m.Close() })
	pkgratelimit.RegisterScripts(m)
	return m
}

func newLimiter(s kv.Store, now *time.Time) *Limiter {
	return New(pkgratelimit.New(s, discardLog(), func() time.Time { return *now }))
}

func admit(t *testing.T, l *Limiter, cs ...limits.Counter) limits.Reservation {
	t.Helper()
	res, err := l.Admit(context.Background(), cs, limits.Usage{})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	return res
}

func refused(t *testing.T, l *Limiter, cs ...limits.Counter) *limits.Refused {
	t.Helper()
	res, err := l.Admit(context.Background(), cs, limits.Usage{})
	var r *limits.Refused
	if !errors.As(err, &r) || res != nil {
		t.Fatalf("Admit = %v, %v; want a refusal and no reservation", res, err)
	}
	return r
}

// failingStore fails the script calls fail picks, numbered from 1, as a store that did not answer.
type failingStore struct {
	kvtest.ScriptStore
	calls atomic.Int64
	fail  func(n int64) bool
}

func (f *failingStore) RunScript(ctx context.Context, name, script string, keys []string, args ...any) ([]byte, error) {
	if f.fail(f.calls.Add(1)) {
		return nil, kv.ErrUnavailable
	}
	return f.ScriptStore.RunScript(ctx, name, script, keys, args...)
}

func runSuite(t *testing.T, store newStore) {
	ctx := context.Background()
	start := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC) // a Wednesday

	t.Run("RollbackAcrossGroups", func(t *testing.T) {
		now := start
		l := newLimiter(store(t), &now)
		lease := callerCounter(rule(ratelimit.MeterConcurrency, ratelimit.StrategySlidingWindow, 1), "project:x")
		bucket := callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyTokenBucket, 2), "project:x")
		upA := upstreamCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, 1), "a")
		upB := upstreamCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, 1), "b")

		admit(t, l, lease, bucket, upA).Settle(limits.Usage{})
		r := refused(t, l, lease, bucket, upA)
		if r.Counter.ID != upA.ID || r.Counter.Rung != limits.RungAccount || r.RetryAfter <= 0 {
			t.Fatalf("refusal = %+v after %v, want account counter %s with a retry-after", r.Counter, r.RetryAfter, upA.ID)
		}
		// The caller group reserved before the account refused; its lease and bucket token must be back.
		held := admit(t, l, lease, bucket, upB)
		if r := refused(t, l, lease, bucket, upB); r.Counter.ID != lease.ID {
			t.Fatalf("refused by %s, want the held lease %s", r.Counter.ID, lease.ID)
		}
		held.Release()
	})

	windows := []ratelimit.Strategy{ratelimit.StrategyFixedWindow, ratelimit.StrategySlidingWindow}

	t.Run("RefusalRefundsWindowCounts", func(t *testing.T) {
		for _, strategy := range windows {
			t.Run(string(strategy), func(t *testing.T) {
				now := start
				l := newLimiter(store(t), &now)
				caller := callerCounter(rule(ratelimit.MeterRequests, strategy, 1), "project:x")
				upA := upstreamCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, 1), "a")
				upB := upstreamCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, 1), "b")
				admit(t, l, upA).Settle(limits.Usage{})

				if r := refused(t, l, caller, upA); r.Counter.ID != upA.ID {
					t.Fatalf("refused by %s, want the full account %s", r.Counter.ID, upA.ID)
				}
				// The caller's one request a minute was reserved before the account refused; it must be back.
				admit(t, l, caller, upB).Settle(limits.Usage{})
				if r := refused(t, l, caller); r.Counter.ID != caller.ID {
					t.Fatalf("refused by %s, want the caller %s", r.Counter.ID, caller.ID)
				}
			})
		}
	})

	t.Run("ReleaseRefundsWindowCounts", func(t *testing.T) {
		for _, strategy := range windows {
			t.Run(string(strategy), func(t *testing.T) {
				now := start
				l := newLimiter(store(t), &now)
				caller := callerCounter(rule(ratelimit.MeterRequests, strategy, 2), "project:x")
				up := upstreamCounter(rule(ratelimit.MeterRequests, strategy, 2), "a")
				for range 3 {
					admit(t, l, caller, up).Release()
				}
				admit(t, l, caller, up)
				admit(t, l, caller, up)
				_ = refused(t, l, caller, up)
			})
		}
	})

	t.Run("SettleKeepsWindowCounts", func(t *testing.T) {
		for _, strategy := range windows {
			t.Run(string(strategy), func(t *testing.T) {
				now := start
				l := newLimiter(store(t), &now)
				caller := callerCounter(rule(ratelimit.MeterRequests, strategy, 1), "project:x")
				admit(t, l, caller).Settle(limits.Usage{Tokens: usage.Tokens{"input": 5}})
				_ = refused(t, l, caller)
			})
		}
	})

	t.Run("ReleaseAfterRolloverIsNoop", func(t *testing.T) {
		now := start
		l := newLimiter(store(t), &now)
		caller := callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, 1), "project:x")
		old := admit(t, l, caller)
		now = now.Add(time.Minute)
		admit(t, l, caller)
		old.Release()
		_ = refused(t, l, caller)
	})

	t.Run("SharedAndDistinctBuckets", func(t *testing.T) {
		now := start
		l := newLimiter(store(t), &now)
		one := rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, 1)
		x, y := callerCounter(one, "project:x"), callerCounter(one, "project:y")
		admit(t, l, x)
		admit(t, l, y)
		if r := refused(t, l, x); r.Counter.ID != x.ID {
			t.Fatalf("refused by %s, want %s", r.Counter.ID, x.ID)
		}

		two := callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, 2), "project:z")
		admit(t, l, two, two) // equal IDs count once
		admit(t, l, two)
		_ = refused(t, l, two)
	})

	t.Run("FailClosed", func(t *testing.T) {
		now := start
		fs := &failingStore{ScriptStore: store(t), fail: func(int64) bool { return true }}
		l := newLimiter(fs, &now)
		c := callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyTokenBucket, 10), "project:x")
		res, err := l.Admit(ctx, []limits.Counter{c}, limits.Usage{})
		var r *limits.Refused
		if res != nil || !errors.Is(err, pkgratelimit.ErrUnavailable) || errors.As(err, &r) {
			t.Fatalf("Admit on a failing store = %v, %v; want ErrUnavailable and no reservation", res, err)
		}

		// The second group's store call fails: the first group's lease must be released.
		fs.calls.Store(0)
		fs.fail = func(n int64) bool { return n == 2 }
		lease := callerCounter(rule(ratelimit.MeterConcurrency, ratelimit.StrategySlidingWindow, 1), "project:x")
		up := upstreamCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyTokenBucket, 10), "a")
		if _, err := l.Admit(ctx, []limits.Counter{lease, up}, limits.Usage{}); !errors.Is(err, pkgratelimit.ErrUnavailable) {
			t.Fatalf("Admit with the account call failing = %v, want ErrUnavailable", err)
		}
		admit(t, l, lease, up)
	})

	t.Run("HardBudget", func(t *testing.T) {
		now := time.Date(2026, 11, 30, 10, 0, 0, 0, time.UTC)
		l := newLimiter(store(t), &now)
		b := budgetCounter("1", budget.PeriodMonth, budget.ModeHard)
		admit(t, l, b).Settle(limits.Usage{CostNanos: 6 * usd / 10})
		admit(t, l, b).Settle(limits.Usage{CostNanos: 6 * usd / 10})
		r := refused(t, l, b)
		if r.Counter.Kind != limits.KindBudget || r.Counter.ID != b.ID || r.RetryAfter != 14*time.Hour {
			t.Fatalf("refusal = %s after %v, want budget %s with retry-after 14h", r.Counter.ID, r.RetryAfter, b.ID)
		}
		now = now.Add(r.RetryAfter)
		admit(t, l, b)
	})

	t.Run("AlertBudget", func(t *testing.T) {
		now := start
		var crossings []string
		l := newLimiter(store(t), &now).OnBudgetCrossed(func(c *limits.Counter) { crossings = append(crossings, c.ID) })
		b := budgetCounter("1", budget.PeriodDay, budget.ModeAlert)
		for range 4 {
			admit(t, l, b).Settle(limits.Usage{CostNanos: 6 * usd / 10})
		}
		if len(crossings) != 1 || crossings[0] != b.ID {
			t.Fatalf("crossings = %v, want %s once", crossings, b.ID)
		}
		now = now.Add(24 * time.Hour)
		admit(t, l, b).Settle(limits.Usage{CostNanos: usd})
		if len(crossings) != 2 {
			t.Fatalf("crossings = %v, want one more in the next period", crossings)
		}
	})

	t.Run("PeriodRollover", func(t *testing.T) {
		for _, tc := range []struct {
			period budget.Period
			retry  time.Duration
		}{
			{budget.PeriodDay, 14 * time.Hour},
			{budget.PeriodWeek, 4*24*time.Hour + 14*time.Hour}, // to Monday the 9th
			{budget.PeriodMonth, 26*24*time.Hour + 14*time.Hour},
		} {
			t.Run(string(tc.period), func(t *testing.T) {
				now := start
				l := newLimiter(store(t), &now)
				b := budgetCounter("1", tc.period, budget.ModeHard)
				admit(t, l, b).Settle(limits.Usage{CostNanos: usd})
				if r := refused(t, l, b); r.RetryAfter != tc.retry {
					t.Fatalf("retry-after = %v, want %v", r.RetryAfter, tc.retry)
				}
				now = now.Add(tc.retry - time.Millisecond)
				_ = refused(t, l, b)
				now = now.Add(time.Millisecond)
				admit(t, l, b).Settle(limits.Usage{CostNanos: usd / 2})
				admit(t, l, b)
			})
		}
	})

	t.Run("OvershootByInFlight", func(t *testing.T) {
		now := start
		l := newLimiter(store(t), &now)
		b := budgetCounter("1", budget.PeriodDay, budget.ModeHard)
		admit(t, l, b).Settle(limits.Usage{CostNanos: 9 * usd / 10})
		a, c := admit(t, l, b), admit(t, l, b)
		a.Settle(limits.Usage{CostNanos: usd / 2})
		c.Settle(limits.Usage{CostNanos: usd / 2})
		_ = refused(t, l, b)
	})

	t.Run("SettleChargesRealCost", func(t *testing.T) {
		now := start
		l := newLimiter(store(t), &now)
		b := budgetCounter("1", budget.PeriodDay, budget.ModeHard)
		res, err := l.Admit(ctx, []limits.Counter{b}, limits.Usage{CostNanos: 50 * usd})
		if err != nil {
			t.Fatalf("an estimate over the cap refused: %v", err)
		}
		res.Settle(limits.Usage{CostNanos: 3 * usd / 10})
		admit(t, l, b).Release()
		admit(t, l, b).Settle(limits.Usage{CostNanos: 7*usd/10 - 1})
		admit(t, l, b).Settle(limits.Usage{CostNanos: 1})
		_ = refused(t, l, b)

		raised := budgetCounter("2", budget.PeriodDay, budget.ModeHard)
		admit(t, l, raised).Settle(limits.Usage{CostNanos: usd})
		_ = refused(t, l, raised)
	})

	t.Run("SettleTrueUpAndLease", func(t *testing.T) {
		now := start
		l := newLimiter(store(t), &now)
		tokens := callerCounter(rule(ratelimit.MeterTokens, ratelimit.StrategySlidingWindow, 100), "project:x")
		lease := upstreamCounter(rule(ratelimit.MeterConcurrency, ratelimit.StrategySlidingWindow, 1), "a")
		admit(t, l, tokens, lease).Settle(limits.Usage{Tokens: usage.Tokens{"input": 80, "output": 70}})
		if r := refused(t, l, tokens, lease); r.Counter.ID != tokens.ID {
			t.Fatalf("refused by %s, want the token meter over its amount", r.Counter.ID)
		}
		admit(t, l, lease) // the settle freed the lease
	})

	t.Run("OneRoundTrip", func(t *testing.T) { admitOneRoundTrip(t, store, start) })

	t.Run("FailedReleaseIsRetried", func(t *testing.T) {
		now := start
		fs := &failingStore{ScriptStore: store(t), fail: func(int64) bool { return false }}
		l := newLimiter(fs, &now)
		lease := callerCounter(rule(ratelimit.MeterConcurrency, ratelimit.StrategySlidingWindow, 1), "project:x")
		full := upstreamCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, 1), "a")
		admit(t, l, full).Settle(limits.Usage{})
		// The admit's two scripts run, then the first release fails: the retry must free the lease.
		fs.calls.Store(0)
		fs.fail = func(n int64) bool { return n == 3 }
		if r := refused(t, l, lease, full); r.Counter.ID != full.ID {
			t.Fatalf("refused by %s, want the full account", r.Counter.ID)
		}
		if n := fs.calls.Load(); n != 4 {
			t.Fatalf("script calls = %d, want the admit's two and two releases", n)
		}
		fs.fail = func(int64) bool { return false }
		admit(t, l, lease)
	})

	t.Run("NoCounters", func(t *testing.T) {
		now := start
		res := admit(t, newLimiter(store(t), &now))
		res.Settle(limits.Usage{CostNanos: usd})
		res.Release()
	})
}

func TestLimiter_Mem(t *testing.T) { runSuite(t, memStore) }

// A group of alert budgets checks nothing at admission, so it costs no store call; Settle commits every group in one round trip.
func TestLimiter_StoreCalls(t *testing.T) {
	rec := kvtest.NewRecording(memStore(t))
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	l := newLimiter(rec, &now)
	caller := callerCounter(rule(ratelimit.MeterConcurrency, ratelimit.StrategySlidingWindow, 5), "project:x")
	alert := limits.BudgetCounter(limits.RungAccount, &budget.Budget{Meta: meta.Metadata{ID: "b2"}, Spec: budget.Spec{AmountUSD: "5", Mode: budget.ModeAlert}}, "p2", "account:a", "account:a")
	res := admit(t, l, caller, alert)
	if n, b := rec.RoundTrips(), rec.Batches(); n != 1 || len(b) != 1 || b[0] != 1 {
		t.Fatalf("Admit: %d round trips, batches %v; want one batch carrying the caller group's script", n, b)
	}
	res.Settle(limits.Usage{CostNanos: usd})
	if n, b := rec.RoundTrips(), rec.Batches(); n != 2 || len(b) != 2 || b[1] != 2 {
		t.Fatalf("Settle: %d round trips total, batches %v; want one batch of two commits", n, b)
	}
}

// admitOneRoundTrip pins that Admit sends every group's script in one round trip, refused or not, and that a refusal in any group leaves every other group's count as it was.
func admitOneRoundTrip(t *testing.T, store newStore, now time.Time) {
	rec := kvtest.NewRecording(store(t))
	l := newLimiter(rec, &now)
	caller := callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, 1), "project:x")
	lease := upstreamCounter(rule(ratelimit.MeterConcurrency, ratelimit.StrategySlidingWindow, 1), "a")
	full := upstreamCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, 1), "b")
	admit(t, l, full).Settle(limits.Usage{})

	trips := rec.RoundTrips()
	if r := refused(t, l, caller, lease, full); r.Counter.ID != full.ID {
		t.Fatalf("refused by %s, want the full account %s", r.Counter.ID, full.ID)
	}
	// One round trip for the three groups, one releasing the two that reserved.
	if n := rec.RoundTrips() - trips; n != 2 {
		t.Fatalf("a refused three-group admit took %d round trips, want 2 (batches %v)", n, rec.Batches())
	}

	trips = rec.RoundTrips()
	res := admit(t, l, caller, lease)
	if n := rec.RoundTrips() - trips; n != 1 {
		t.Fatalf("a two-group admit took %d round trips, want 1", n)
	}
	// The caller is refused while the account lease is free: the caller's refusal is reported and the lease reserved beside it is released again.
	if r := refused(t, l, caller, upstreamCounter(rule(ratelimit.MeterConcurrency, ratelimit.StrategySlidingWindow, 1), "c")); r.Counter.ID != caller.ID {
		t.Fatalf("refused by %s, want the caller %s", r.Counter.ID, caller.ID)
	}
	res.Release()
	admit(t, l, caller, upstreamCounter(rule(ratelimit.MeterConcurrency, ratelimit.StrategySlidingWindow, 1), "c"))
}

func TestGroupCounters_Order(t *testing.T) {
	r := rule(ratelimit.MeterRequests, ratelimit.StrategyTokenBucket, 1)
	other := limits.UpstreamRule(limits.RungAccount, testRateLimit, r, "p3", "account:b", "", "account:b")
	cs := []limits.Counter{upstreamCounter(r, "a"), other, callerCounter(r, "project:x"), upstreamCounter(r, "a")}
	groups, err := groupCounters(cs)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, g := range groups {
		got = append(got, g.name)
	}
	if len(got) != 3 || got[0] != "team:t1" || got[1] != "account:a" || got[2] != "account:b" || groups[0].upstream || !groups[1].upstream {
		t.Fatalf("groups = %v, want team:t1 (caller), account:a, account:b", got)
	}
	if len(groups[1].rules) != 1 {
		t.Fatalf("account group holds %d rules, want the duplicate dropped", len(groups[1].rules))
	}
}

func TestUSDNanos(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"0", 0, true}, {"2000", 2000 * usd, true}, {"12.5", 12*usd + usd/2, true}, {"0.01", usd / 100, true},
		{"", 0, false}, {"-1", 0, false}, {"1.-5", 0, false}, {"1.0000000001", 0, false}, {"abc", 0, false}, {"99999999999", 0, false},
	} {
		got, err := usdNanos(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("usdNanos(%q) = %d, %v; want %d ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}
