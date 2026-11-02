package ratelimit

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
)

// runPlainMatchesScript writes counts into the keys CounterKey names and checks that CheckPlain decides a reserve, retry-after included, as the reserve script does on those keys.
func runPlainMatchesScript(t *testing.T, newStore func(t *testing.T) kvtest.ScriptStore) {
	ctx := context.Background()
	s := newStore(t)
	rules := []Rule{
		{Meter: "requests", Strategy: StrategyFixedWindow, Amount: 100, Window: time.Minute},
		{Meter: "requests", Strategy: StrategySlidingWindow, Amount: 100, Window: time.Minute},
		{Meter: "requests", Strategy: StrategySlidingWindow, Amount: 100, Window: 7 * time.Second},
		{Meter: "tokens.input", Strategy: StrategyTokenBucket, Amount: 1000, Window: time.Minute},
		{Meter: MeterSpend, Amount: 5000, Period: PeriodWeek},
		{Meter: MeterSpend, Amount: 5000, Period: PeriodDay, Alert: true},
	}
	counts := [][2]int64{{0, 0}, {99, 0}, {100, 0}, {60, 80}, {40, 120}, {1000, 0}, {700, 600}, {4999, 0}, {5000, 0}, {0, 300}}
	offsets := []time.Duration{0, 1500 * time.Millisecond, 29 * time.Second, 59*time.Second + 999*time.Millisecond}
	base := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	n := 0
	for ri, r := range rules {
		for _, c := range counts {
			for _, off := range offsets {
				n++
				r.Key = "plain:" + strconv.Itoa(n)
				r.Name = r.Key
				now := base.Add(off)
				l := New(s, discardLog(), func() time.Time { return now })
				cur, err := BucketStart(r, now)
				if err != nil {
					t.Fatal(err)
				}
				prev, _ := BucketStart(r, cur.Add(-time.Millisecond))
				setCount(t, s, "scope", r, cur, now, c[0])
				setCount(t, s, "scope", r, prev, now, c[1])

				retry, fits := CheckPlain(r, c[0], c[1], now)
				_, err = l.Reserve(ctx, "scope", []Rule{r})
				var ex *KeyQuotaExhausted
				switch {
				case err == nil && !fits:
					t.Errorf("rule %d counts %v at +%v: script admits, CheckPlain refuses (retry %v)", ri, c, off, retry)
				case errors.As(err, &ex) && fits:
					t.Errorf("rule %d counts %v at +%v: script refuses, CheckPlain admits", ri, c, off)
				case errors.As(err, &ex) && ex.RetryAfter != retry:
					t.Errorf("rule %d counts %v at +%v: retry-after %v, CheckPlain %v", ri, c, off, ex.RetryAfter, retry)
				case err != nil && !errors.As(err, &ex):
					t.Fatalf("Reserve: %v", err)
				}
			}
		}
	}
}

func setCount(t *testing.T, s kv.Store, scope string, r Rule, start, now time.Time, n int64) {
	t.Helper()
	key, ttl, err := CounterKey(scope, r, start, now)
	if err != nil {
		t.Fatal(err)
	}
	if ttl <= 0 {
		return // a closed period, which nothing reads
	}
	if err := s.Set(context.Background(), key, []byte(strconv.FormatInt(n, 10)), ttl); err != nil {
		t.Fatal(err)
	}
}

func TestPlainMatchesScript_Mem(t *testing.T) {
	runPlainMatchesScript(t, func(t *testing.T) kvtest.ScriptStore {
		m := kv.NewMem()
		t.Cleanup(func() { _ = m.Close() })
		RegisterScripts(m)
		return m
	})
}

func TestPlainCounterOf(t *testing.T) {
	for _, tc := range []struct {
		r    Rule
		want PlainCounter
	}{
		{Rule{Meter: "requests", Strategy: StrategyFixedWindow}, PlainFixedWindow},
		{Rule{Meter: "requests", Strategy: StrategySlidingWindow}, PlainSlidingWindow},
		{Rule{Meter: "tokens", Strategy: StrategyTokenBucket}, PlainTokens},
		{Rule{Meter: "tokens.output", Strategy: StrategyFixedWindow}, PlainTokens},
		{Rule{Meter: MeterSpend}, PlainSpend},
		{Rule{Meter: "requests", Strategy: StrategyTokenBucket}, NotPlain},
		{Rule{Meter: "requests", Strategy: StrategyLeakyBucket}, NotPlain},
		{Rule{Meter: "requests", Strategy: StrategySessionWindow}, NotPlain},
		{Rule{Meter: "concurrency", Strategy: StrategyFixedWindow}, NotPlain},
		{Rule{Meter: MeterRevoked}, NotPlain},
	} {
		if got := PlainCounterOf(tc.r); got != tc.want {
			t.Errorf("PlainCounterOf(%s %s) = %d, want %d", tc.r.Meter, tc.r.Strategy, got, tc.want)
		}
	}
}

func TestTokenCharge(t *testing.T) {
	tokens := map[string]int64{"input": 300, "output": 200, "cache_read": 50}
	for meter, want := range map[string]int64{"tokens": 500, "tokens.cache_read": 50, "tokens.reasoning": 0, "requests": 0} {
		if got := TokenCharge(meter, tokens); got != want {
			t.Errorf("TokenCharge(%s) = %d, want %d", meter, got, want)
		}
	}
}
