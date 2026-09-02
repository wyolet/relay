package ratelimit

// scope_contract_test.go — the second parameterised contract suite: what the
// revoked meter and the scope hash tag must do identically on every backend.
// The mem half runs unconditionally here; the Redis half is the same suite
// called from the integration-tagged file, so a Lua/emulator divergence in
// the token-revocation path shows up as a failure rather than as a mem-only
// green.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func revokedRule(jti string) Rule {
	return Rule{Key: "jti:" + jti, Name: "token revocation", Meter: MeterRevoked}
}

func runScopeContractSuite(t *testing.T, name string, factory func(t *testing.T, now *time.Time) *Limiter) {
	ctx := context.Background()

	t.Run(name+"/Revoked_BeatsAnExhaustedRule", func(t *testing.T) {
		now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
		l := factory(t, &now)
		scope := "team:" + name + "-order"
		// A cap of 0 always violates, so whichever rule the backend reports
		// tells us which one it evaluated first. Revocation has to win: the
		// caller must see 401, not the 429 an over-limit rule would produce.
		full := Rule{
			Key: "rl-order-full", Name: "requests", Meter: "requests",
			Strategy: StrategySlidingWindow, Amount: 0, Window: time.Minute,
		}
		if err := l.store.Set(ctx, "limit:{"+scope+"}:jti:order-jti", []byte("1"), time.Minute); err != nil {
			t.Fatalf("write denylist entry: %v", err)
		}
		_, err := l.Reserve(ctx, scope, []Rule{revokedRule("order-jti"), full})
		if !errors.Is(err, ErrRevoked) {
			t.Fatalf("err = %v, want ErrRevoked ahead of the exhausted rule", err)
		}
	})

	t.Run(name+"/Revoked_WritesNoCounter", func(t *testing.T) {
		now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
		l := factory(t, &now)
		scope := "team:" + name + "-nowrite"
		if _, err := l.Reserve(ctx, scope, []Rule{revokedRule("nowrite-jti")}); err != nil {
			t.Fatalf("reserve: %v", err)
		}
		// The revocation check is a read. A counter written here would be
		// state the commit path is entitled to skip, and would leak per jti.
		for _, e := range keysUnder(t, l, scope) {
			t.Errorf("revocation-only reserve wrote %q", e)
		}
	})

	t.Run(name+"/Revoked_IsScopedToOneTeam", func(t *testing.T) {
		now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
		l := factory(t, &now)
		mine := "team:" + name + "-mine"
		theirs := "team:" + name + "-theirs"
		jti := "shared-jti"
		if err := l.store.Set(ctx, "limit:{"+mine+"}:jti:"+jti, []byte("1"), time.Minute); err != nil {
			t.Fatalf("write denylist entry: %v", err)
		}
		if _, err := l.Reserve(ctx, mine, []Rule{revokedRule(jti)}); !errors.Is(err, ErrRevoked) {
			t.Fatalf("own team: err = %v, want ErrRevoked", err)
		}
		// Same jti, another team's tag: one team's revocation must not reach
		// into another's slot.
		if _, err := l.Reserve(ctx, theirs, []Rule{revokedRule(jti)}); err != nil {
			t.Fatalf("other team: %v", err)
		}
	})

	t.Run(name+"/TeamTag_CoversEveryKeyOfOneReserve", func(t *testing.T) {
		now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
		l := factory(t, &now)
		scope := "team:" + name + "-tag"
		rules := []Rule{
			revokedRule("tag-jti"),
			{Key: "rl-tag-req", Name: "requests", Meter: "requests", Strategy: StrategySlidingWindow, Amount: 100, Window: time.Minute},
			{Key: "rl-tag-tok", Name: "tokens", Meter: "tokens", Strategy: StrategySlidingWindow, Amount: 1000, Window: time.Minute},
			{Key: "rl-tag-con", Name: "concurrency", Meter: "concurrency", Strategy: StrategySlidingWindow, Amount: 10, Window: time.Minute},
		}
		res, err := l.Reserve(ctx, scope, rules)
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		if err := l.Commit(ctx, res, Observations{Tokens: map[string]int64{"tokens": 5}}); err != nil {
			t.Fatalf("commit: %v", err)
		}
		// Every key one request touches has to sit in the same Cluster slot,
		// or the single Reserve script is a CROSSSLOT error on a real cluster.
		want := "limit:{" + scope + "}:"
		written := keysUnder(t, l, scope)
		if len(written) == 0 {
			t.Fatal("reserve+commit wrote no counters at all")
		}
		for _, k := range written {
			if !strings.HasPrefix(k, want) {
				t.Errorf("key %q is outside the team hash tag %q", k, want)
			}
		}
	})

	t.Run(name+"/TeamTag_SeparatesTwoTeamsOnTheSameRule", func(t *testing.T) {
		now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
		l := factory(t, &now)
		rule := Rule{
			Key: "rl-split-req", Name: "requests", Meter: "requests",
			Strategy: StrategySlidingWindow, Amount: 1, Window: time.Minute,
		}
		first := "team:" + name + "-split-a"
		second := "team:" + name + "-split-b"
		res, err := l.Reserve(ctx, first, []Rule{rule})
		if err != nil {
			t.Fatalf("first team reserve: %v", err)
		}
		_ = l.Commit(ctx, res, Observations{})
		if _, err := l.Reserve(ctx, first, []Rule{rule}); !errors.Is(err, ErrExceeded) {
			t.Fatalf("first team second reserve: err = %v, want ErrExceeded", err)
		}
		// The identical rule key under another team is a separate budget.
		if _, err := l.Reserve(ctx, second, []Rule{rule}); err != nil {
			t.Fatalf("second team reserve: %v", err)
		}
	})
}

// keysUnder lists the counters written under one scope's hash tag.
func keysUnder(t *testing.T, l *Limiter, scope string) []string {
	t.Helper()
	entries, err := l.store.Range(context.Background(), "limit:")
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.Contains(e.Key, scope) {
			out = append(out, e.Key)
		}
	}
	return out
}

func TestContractScope_MemStore(t *testing.T) {
	runScopeContractSuite(t, "MemStore", memLimiterFactory)
}
