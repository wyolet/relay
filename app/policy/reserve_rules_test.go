package policy

import (
	"context"
	"testing"
	"time"

	appratelimit "github.com/wyolet/relay/app/ratelimit"
)

// The revocation rule must stay first in the slice — a 401 for a revoked
// token beats the 429 an over-limit rule ahead of it would produce — and the
// slice is pre-sized so the metered rules never force a regrow.
func TestReserveInbound_RevokedRuleStaysFirst(t *testing.T) {
	svc, store, pol := reserveFixture(t,
		appratelimit.Rule{Meter: appratelimit.MeterRequests, Amount: 10, Window: appratelimit.Window(time.Minute), Strategy: appratelimit.StrategyFixedWindow},
		appratelimit.Rule{Meter: appratelimit.MeterTokens, Amount: 100, Window: appratelimit.Window(time.Minute), Strategy: appratelimit.StrategyFixedWindow},
	)
	if _, err := svc.ReserveInbound(context.Background(), InboundInput{
		Policy: pol, TeamID: "team-1", TokenJTI: "jti-1",
	}); err != nil {
		t.Fatalf("ReserveInbound: %v", err)
	}
	keys := store.lastKeys()
	if len(keys) == 0 || keys[0] != "limit:{team:team-1}:jti:jti-1" {
		t.Fatalf("first key = %v, want the revocation key first", keys)
	}
	if got := store.reserveCalls(); got != 1 {
		t.Fatalf("reserve scripts = %d, want exactly 1", got)
	}
}

// One request costs one script call no matter how many rules the policy
// carries — the hot-path budget.
func TestReserveInbound_OneScriptRegardlessOfRuleCount(t *testing.T) {
	rules := make([]appratelimit.Rule, 0, 6)
	for i := 0; i < 6; i++ {
		rules = append(rules, appratelimit.Rule{
			Meter: appratelimit.MeterRequests, Amount: 1 << 20, Window: appratelimit.Window(time.Hour),
			Strategy: appratelimit.StrategyFixedWindow,
		})
	}
	svc, store, pol := reserveFixture(t, rules...)
	if _, err := svc.ReserveInbound(context.Background(), InboundInput{
		Policy: pol, TeamID: "team-1", TokenJTI: "jti-1",
	}); err != nil {
		t.Fatalf("ReserveInbound: %v", err)
	}
	if got := store.reserveCalls(); got != 1 {
		t.Fatalf("reserve scripts = %d, want exactly 1", got)
	}
}
