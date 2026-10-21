package policy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appratelimit "github.com/wyolet/relay/app/ratelimit"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

func ceilingRules(amount int64) []pkgratelimit.Rule {
	rl := &appratelimit.RateLimit{}
	rl.Spec.Rules = []appratelimit.Rule{{
		Meter: appratelimit.MeterRequests, Amount: amount, Window: appratelimit.Window(time.Hour),
		Strategy: appratelimit.StrategyFixedWindow,
	}}
	return appratelimit.ResolveWithScope("proxy", "keyhash", rl)
}

// Extra rules ride the policy's single Reserve, under its hash tag, after the
// revocation check.
func TestReserveInbound_ExtraRulesShareTheOneScript(t *testing.T) {
	svc, store, pol := reserveFixture(t, appratelimit.Rule{
		Meter: appratelimit.MeterRequests, Amount: 10, Window: appratelimit.Window(time.Minute),
		Strategy: appratelimit.StrategyFixedWindow,
	})

	if _, err := svc.ReserveInbound(context.Background(), InboundInput{
		Policy: pol, TeamID: "team-1", TokenJTI: "jti-1", ExtraRules: ceilingRules(10),
	}); err != nil {
		t.Fatalf("ReserveInbound: %v", err)
	}
	if got := reserveCalls(store); got != 1 {
		t.Fatalf("reserve scripts = %d, want exactly 1", got)
	}
	keys := lastKeys(store)
	if len(keys) != 3 || keys[0] != RevokedKey("team-1", "jti-1") {
		t.Fatalf("script keys = %v, want the revocation key first, then the policy and extra rule keys", keys)
	}
	var sawPolicy, sawExtra bool
	for _, k := range keys {
		if !strings.HasPrefix(k, "limit:{team:team-1}:") {
			t.Errorf("key %q is not under the team hash tag", k)
		}
		sawPolicy = sawPolicy || strings.Contains(k, ":policy:")
		sawExtra = sawExtra || strings.Contains(k, ":proxy:keyhash:")
	}
	if !sawPolicy || !sawExtra {
		t.Fatalf("script keys = %v, want both the policy rule and the extra rule", keys)
	}
}

// Extra rules alone are metered state: the reservation exists and its commit
// is a real script call, not skipped as a check-only reservation. (A
// cancelled commit, since an uncancelled one of request-only rules settles
// nothing either way.)
func TestReserveInbound_ExtraRulesAloneAreCommitted(t *testing.T) {
	svc, store, pol := reserveFixture(t)

	res, err := svc.ReserveInbound(context.Background(), InboundInput{Policy: pol, ExtraRules: ceilingRules(10)})
	if err != nil {
		t.Fatalf("ReserveInbound: %v", err)
	}
	if res == nil {
		t.Fatal("reservation is nil, want one covering the extra rules")
	}
	if err := svc.CommitInbound(context.Background(), res, pkgratelimit.Observations{Cancelled: true}); err != nil {
		t.Fatalf("CommitInbound: %v", err)
	}
	if got := len(store.Named("limit.commit")); got != 1 {
		t.Fatalf("commit scripts = %d (%v), want 1", got, scriptNames(store))
	}
}

// The tighter of the policy's rule and an extra rule refuses.
func TestReserveInbound_ExtraRuleIsACeiling(t *testing.T) {
	svc, _, pol := reserveFixture(t, appratelimit.Rule{
		Meter: appratelimit.MeterRequests, Amount: 100, Window: appratelimit.Window(time.Hour),
		Strategy: appratelimit.StrategyFixedWindow,
	})
	in := InboundInput{Policy: pol, ExtraRules: ceilingRules(1)}

	if _, err := svc.ReserveInbound(context.Background(), in); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := svc.ReserveInbound(context.Background(), in)
	var exceeded *pkgratelimit.KeyQuotaExhausted
	if !errors.As(err, &exceeded) {
		t.Fatalf("second: err = %v, want the extra rule's KeyQuotaExhausted", err)
	}
}
