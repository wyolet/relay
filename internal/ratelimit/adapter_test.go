package ratelimit

import (
	"fmt"
	"testing"
	"time"

	"github.com/wyolet/relay/internal/catalog"
	pkgrl "github.com/wyolet/relay/pkg/ratelimit"
)

// TestAdapterKeyFormat verifies that toRules produces pkg.Rule.Key values that
// match the wire format "Kind:parentName:rlName". This is load-bearing: any
// drift here changes which Redis keys are written and breaks existing counters.
func TestAdapterKeyFormat(t *testing.T) {
	rule := catalog.ResolvedRule{
		ParentKind:    catalog.KindPolicy,
		ParentName:    "prod-policy",
		RateLimitName: "rl-basic",
		Strategy:      catalog.StrategyTokenBucket,
		Window:        time.Minute,
		Meter:         catalog.MeterRequests,
		Rule: catalog.RateLimitRule{
			Meter:    "requests",
			Amount:   100,
			Strategy: catalog.StrategyTokenBucket,
		},
		RateLimit: &catalog.RateLimit{
			Metadata: catalog.Metadata{Name: "rl-basic"},
			Spec: catalog.RateLimitSpec{
				Strategy: catalog.StrategyTokenBucket,
				Window:   time.Minute,
				Rules:    []catalog.RateLimitRule{{Meter: "requests", Amount: 100}},
			},
		},
	}

	pkgRules := toRules([]catalog.ResolvedRule{rule})
	if len(pkgRules) != 1 {
		t.Fatalf("expected 1 pkg rule, got %d", len(pkgRules))
	}

	got := pkgRules[0].Key
	want := fmt.Sprintf("%s:%s:%s", catalog.KindPolicy, "prod-policy", "rl-basic")
	if got != want {
		t.Errorf("key mismatch\n  got:  %q\n  want: %q", got, want)
	}

	// Strategy must propagate correctly.
	if pkgRules[0].Strategy != pkgrl.StrategyTokenBucket {
		t.Errorf("strategy mismatch: got %q, want %q", pkgRules[0].Strategy, pkgrl.StrategyTokenBucket)
	}

	// Scope passed to pkg Reserve is "policy:<poolName>"; the full Redis hash-tag
	// key becomes "limit:{policy:<poolName>}:tb:<ruleKey>:<meter>".
	// Verify the key is deterministic by calling toRules again.
	pkgRules2 := toRules([]catalog.ResolvedRule{rule})
	if pkgRules2[0].Key != pkgRules[0].Key {
		t.Error("toRules is not deterministic")
	}
}
