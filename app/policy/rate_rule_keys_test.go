package policy

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	appratelimit "github.com/wyolet/relay/app/ratelimit"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// D72: the rule key names the RateLimit and, for a per-model binding,
// the model, so two bindings of one policy never share a bucket.
func TestRuleKeyFormat_CarriesRateLimitAndModel(t *testing.T) {
	rl := &appratelimit.RateLimit{}
	rl.Meta.ID, rl.Meta.Name = "rl-a", "rl-a"
	rl.Spec.Rules = []appratelimit.Rule{{
		Meter: appratelimit.MeterRequests, Amount: 10,
		Window: appratelimit.Window(time.Minute), Strategy: appratelimit.StrategyFixedWindow,
	}}
	pol := fix("prod-policy")

	flat := pol.ResolveRules(rl, "")
	if len(flat) != 1 || flat[0].Key != "policy:prod-policy:rl:rl-a:0:requests" {
		t.Fatalf("flat key = %v, want policy:prod-policy:rl:rl-a:0:requests", keysOf(flat))
	}
	perModel := pol.ResolveRules(rl, "model-1")
	if len(perModel) != 1 || perModel[0].Key != "policy:prod-policy:rl:rl-a:m:model-1:0:requests" {
		t.Fatalf("per-model key = %v, want policy:prod-policy:rl:rl-a:m:model-1:0:requests", keysOf(perModel))
	}
}

// Two RLBindings of one policy meter into distinct buckets: before the
// key carried the RateLimit and model, both rendered the same string.
func TestReserveInbound_PerModelBindingsGetDistinctBuckets(t *testing.T) {
	rule := appratelimit.Rule{
		Meter: appratelimit.MeterRequests, Amount: 1 << 20,
		Window: appratelimit.Window(time.Hour), Strategy: appratelimit.StrategyFixedWindow,
	}
	rlA := &appratelimit.RateLimit{}
	rlA.Meta.ID, rlA.Meta.Name = "rl-a", "rl-a"
	rlA.Spec.Rules = []appratelimit.Rule{rule}
	rlB := &appratelimit.RateLimit{}
	rlB.Meta.ID, rlB.Meta.Name = "rl-b", "rl-b"
	rlB.Spec.Rules = []appratelimit.Rule{rule}

	pol := fix("prod-policy")
	pol.Meta.ID = "pol-1"
	pol.Spec.RLBindings = []RLBinding{
		{Models: []string{"prov/fast"}, RateLimitID: "rl-a"},
		{Models: []string{"prov/slow"}, RateLimitID: "rl-b"},
	}
	store := newCountingKV()
	t.Cleanup(func() { _ = store.Close() })
	svc := NewService(
		multiRLSnap{pol: pol, rls: map[string]*appratelimit.RateLimit{"rl-a": rlA, "rl-b": rlB}},
		nil, pkgratelimit.New(store, slog.New(slog.NewTextHandler(io.Discard, nil)), nil))

	ctx := context.Background()
	if _, err := svc.ReserveInbound(ctx, InboundInput{
		Policy: pol, ProviderSlug: "prov", ModelSlug: "fast", ModelID: "model-fast",
	}); err != nil {
		t.Fatalf("reserve fast: %v", err)
	}
	fast := store.lastKeys()
	if _, err := svc.ReserveInbound(ctx, InboundInput{
		Policy: pol, ProviderSlug: "prov", ModelSlug: "slow", ModelID: "model-slow",
	}); err != nil {
		t.Fatalf("reserve slow: %v", err)
	}
	slow := store.lastKeys()

	if len(fast) == 0 || len(slow) == 0 {
		t.Fatalf("no keys recorded: fast=%v slow=%v", fast, slow)
	}
	for _, k := range fast {
		for _, other := range slow {
			if k == other {
				t.Fatalf("bindings share bucket %q", k)
			}
		}
	}
	t.Logf("fast binding buckets: %v", fast)
	t.Logf("slow binding buckets: %v", slow)
	if !strings.Contains(fast[0], ":m:model-fast:") {
		t.Fatalf("fast key %q does not carry the model id", fast[0])
	}
}

func keysOf(rules []pkgratelimit.Rule) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.Key)
	}
	return out
}

// multiRLSnap answers a policy carrying several rate limits.
type multiRLSnap struct {
	pol *Policy
	rls map[string]*appratelimit.RateLimit
}

func (s multiRLSnap) Policy(context.Context, string) (*Policy, bool) { return s.pol, s.pol != nil }
func (s multiRLSnap) RateLimit(_ context.Context, id string) (*appratelimit.RateLimit, bool) {
	rl, ok := s.rls[id]
	return rl, ok
}
