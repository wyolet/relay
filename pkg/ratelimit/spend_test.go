package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
)

func TestPeriodBounds(t *testing.T) {
	at := time.Date(2026, 11, 4, 15, 30, 0, 0, time.FixedZone("UTC+5", 5*3600)) // Wednesday 10:30 UTC
	for _, tc := range []struct {
		p          Period
		start, end string
	}{
		{PeriodDay, "2026-11-04", "2026-11-05"},
		{PeriodWeek, "2026-11-02", "2026-11-09"},
		{PeriodMonth, "2026-11-01", "2026-12-01"},
	} {
		start, end, err := periodBounds(tc.p, at)
		if err != nil || start.Format(time.DateOnly) != tc.start || end.Format(time.DateOnly) != tc.end || start.Location() != time.UTC {
			t.Errorf("%s: got %v..%v (%v), want %s..%s UTC", tc.p, start, end, err, tc.start, tc.end)
		}
	}
	if start, _, _ := periodBounds(PeriodWeek, time.Date(2026, 11, 8, 23, 0, 0, 0, time.UTC)); start.Day() != 2 {
		t.Errorf("a Sunday belongs to the week starting Monday the 2nd, got %v", start)
	}
	if _, _, err := periodBounds("year", at); err == nil {
		t.Error("an unknown period was accepted")
	}
}

func spendRule(key string, capNanos int64, alert bool) Rule {
	return Rule{Key: key, Name: key, Meter: MeterSpend, Amount: capNanos, Period: PeriodDay, Alert: alert}
}

func runSpendSuite(t *testing.T, name string, factory func(t *testing.T, now *time.Time) *Limiter) {
	ctx := context.Background()

	t.Run(name+"/HardRefusesAtCap", func(t *testing.T) {
		now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
		l := factory(t, &now)
		rules := []Rule{spendRule("b1", 100, false)}
		for _, cost := range []int64{60, 40} {
			res, err := l.Reserve(ctx, "s", rules)
			if err != nil {
				t.Fatalf("reserve under the cap: %v", err)
			}
			if _, err := l.CommitAll(ctx, Observations{CostNanos: cost}, res); err != nil {
				t.Fatalf("commit: %v", err)
			}
		}
		_, err := l.Reserve(ctx, "s", rules)
		var ex *KeyQuotaExhausted
		if !errors.As(err, &ex) || ex.Rule.Key != "b1" || ex.RetryAfter != 14*time.Hour {
			t.Fatalf("reserve at the cap = %v, want refusal of b1 with retry-after 14h", err)
		}
		now = now.Add(14 * time.Hour)
		if _, err := l.Reserve(ctx, "s", rules); err != nil {
			t.Fatalf("reserve in the next period: %v", err)
		}
	})

	t.Run(name+"/CrossingReportedOnce", func(t *testing.T) {
		now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
		l := factory(t, &now)
		rules := []Rule{spendRule("b2", 100, true), spendRule("b3", 1000, false)}
		var reports []string
		for _, cost := range []int64{90, 20, 20} {
			res, err := l.Reserve(ctx, "s", rules)
			if err != nil {
				t.Fatalf("reserve: %v", err)
			}
			crossed, err := l.CommitAll(ctx, Observations{CostNanos: cost}, res)
			if err != nil {
				t.Fatalf("commit: %v", err)
			}
			for _, r := range crossed {
				reports = append(reports, r.Key)
			}
		}
		if len(reports) != 1 || reports[0] != "b2" {
			t.Fatalf("crossings = %v, want b2 once", reports)
		}
	})

	t.Run(name+"/CancelledChargesNothing", func(t *testing.T) {
		now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
		l := factory(t, &now)
		rules := []Rule{spendRule("b4", 100, false)}
		res, _ := l.Reserve(ctx, "s", rules)
		if _, err := l.CommitAll(ctx, Observations{CostNanos: 500, Cancelled: true}, res); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if _, err := l.Reserve(ctx, "s", rules); err != nil {
			t.Fatalf("a cancelled commit charged the budget: %v", err)
		}
	})
}

func TestSpend_Mem(t *testing.T) { runSpendSuite(t, "MemStore", newTestLimiter) }

// An alert-only reserve checks nothing, so it costs no store call; its commit still charges.
func TestSpend_AlertOnlyReserveMakesNoCall(t *testing.T) {
	ctx := context.Background()
	mem := kv.NewMem()
	RegisterScripts(mem)
	rec := kvtest.NewRecording(mem)
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	l := New(rec, discardLog(), func() time.Time { return now })
	res, err := l.Reserve(ctx, "s", []Rule{spendRule("b5", 100, true)})
	if err != nil || rec.RoundTrips() != 0 {
		t.Fatalf("alert-only reserve = %v after %d round trips, want none", err, rec.RoundTrips())
	}
	crossed, err := l.CommitAll(ctx, Observations{CostNanos: 150}, res)
	if err != nil || len(crossed) != 1 || rec.RoundTrips() != 1 {
		t.Fatalf("commit = %v, crossed %v, %d round trips; want one crossing in one trip", err, crossed, rec.RoundTrips())
	}
	if _, err := l.Reserve(ctx, "s", []Rule{{Key: "x", Meter: MeterSpend, Period: "year", Alert: true}}); err == nil {
		t.Fatal("an alert rule with an unknown period was accepted")
	}
}

// A reservation without spend rules commits with the ten arguments it always sent.
func TestSpend_CommitArgsUnchangedWithoutSpend(t *testing.T) {
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	l := newTestLimiter(t, &now)
	res, err := l.Reserve(context.Background(), "s", []Rule{{Key: "c", Meter: "concurrency", Amount: 1, Window: time.Minute}})
	if err != nil {
		t.Fatal(err)
	}
	call, err := l.buildCommitCall(res, Observations{CostNanos: 10}, now)
	if err != nil || len(call.Args) != 10 {
		t.Fatalf("commit args = %d (%v), want 10", len(call.Args), err)
	}
}
