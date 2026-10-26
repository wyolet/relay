package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
)

// runCommitSkipSuite pins which commits reach the store: an uncancelled commit of request-only rules settles nothing and makes no call (no guard key either); concurrency and token rules, and a cancelled request's refund, still commit.
func runCommitSkipSuite(t *testing.T, newStore func(t *testing.T) kvtest.ScriptStore) {
	newLimiter := func(t *testing.T) (*Limiter, *kvtest.Recording) {
		rec := kvtest.NewRecording(newStore(t))
		return New(rec, discardLog(), nil), rec
	}
	ctx := context.Background()
	tbRule := func(key string) Rule {
		return Rule{Key: key, Name: "requests", Meter: "requests", Strategy: StrategyTokenBucket, Amount: 1, Window: time.Hour}
	}

	t.Run("request_only_makes_no_call", func(t *testing.T) {
		l, rec := newLimiter(t)
		rules := []Rule{
			reqRule("skip:sliding", "sliding", 100, time.Minute),
			{Key: "skip:fixed", Name: "fixed", Meter: "requests", Strategy: StrategyFixedWindow, Amount: 100, Window: time.Minute},
			tbRule("skip:bucket"),
		}
		res, err := l.Reserve(ctx, "skip-scope", rules)
		if err != nil {
			t.Fatal(err)
		}
		other, err := l.Reserve(ctx, "skip-other", rules[:1])
		if err != nil {
			t.Fatal(err)
		}
		before := rec.RoundTrips()
		obs := Observations{Tokens: map[string]int64{"input": 5, "output": 5}}
		if err := l.Commit(ctx, res, obs); err != nil {
			t.Fatal(err)
		}
		if err := l.CommitBoth(ctx, res, other, obs); err != nil {
			t.Fatal(err)
		}
		b := kv.NewBatch(rec, 1)
		if pc, _ := l.PrepareCommit(b, res, obs); pc != nil {
			t.Fatal("PrepareCommit queued a commit that settles nothing")
		}
		if trips := rec.RoundTrips() - before; trips != 0 || len(rec.Named("limit.commit")) != 0 {
			t.Fatalf("round trips = %d, commit scripts = %d; want none", trips, len(rec.Named("limit.commit")))
		}
		if _, err := rec.Get(ctx, commitGuardKey("skip-scope", res.ID)); !errors.Is(err, kv.ErrNotFound) {
			t.Fatalf("guard key: %v, want none written", err)
		}
	})

	t.Run("concurrency_still_released", func(t *testing.T) {
		l, rec := newLimiter(t)
		rules := []Rule{conRule("skip:con", "concurrency", 1, time.Minute)}
		res, err := l.Reserve(ctx, "skip-con", rules)
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Commit(ctx, res, Observations{}); err != nil {
			t.Fatal(err)
		}
		if n := len(rec.Named("limit.commit")); n != 1 {
			t.Fatalf("commit scripts = %d, want 1", n)
		}
		if _, err := l.Reserve(ctx, "skip-con", rules); err != nil {
			t.Fatalf("slot not released: %v", err)
		}
	})

	t.Run("tokens_still_counted", func(t *testing.T) {
		l, rec := newLimiter(t)
		rules := []Rule{tokRule("skip:tok", "tokens", 10, time.Hour)}
		res, err := l.Reserve(ctx, "skip-tok", rules)
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Commit(ctx, res, Observations{Tokens: map[string]int64{"input": 500, "output": 500}}); err != nil {
			t.Fatal(err)
		}
		if n := len(rec.Named("limit.commit")); n != 1 {
			t.Fatalf("commit scripts = %d, want 1", n)
		}
		if _, err := l.Reserve(ctx, "skip-tok", rules); !errors.Is(err, ErrExceeded) {
			t.Fatalf("after 1000 tokens against 10: %v, want ErrExceeded", err)
		}
	})

	t.Run("cancelled_still_refunds", func(t *testing.T) {
		l, rec := newLimiter(t)
		rules := []Rule{tbRule("skip:refund")}
		res, err := l.Reserve(ctx, "skip-refund", rules)
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Commit(ctx, res, Observations{Cancelled: true}); err != nil {
			t.Fatal(err)
		}
		if n := len(rec.Named("limit.commit")); n != 1 {
			t.Fatalf("commit scripts = %d, want 1", n)
		}
		if _, err := l.Reserve(ctx, "skip-refund", rules); err != nil {
			t.Fatalf("bucket not refunded: %v", err)
		}
	})
}

func TestCommitSkipMem(t *testing.T) {
	runCommitSkipSuite(t, func(t *testing.T) kvtest.ScriptStore {
		m := newTestStore(t)
		RegisterScripts(m)
		return m
	})
}
