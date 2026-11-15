package catalogview

import (
	"context"
	"testing"

	"github.com/wyolet/relay/pkg/meta"
)

// A linked rate limit the caller may not see (an organization's, linked by an admin) is listed by id with no name or rules, and its rules stay out of every per-model limit list.
func TestHiddenRateLimits(t *testing.T) {
	ctx := context.Background()
	p := limitedBy(limitedBy(policyOf("p", operator, nil), "rl1"), "org-rl", "openai/gpt-4o")
	svc := newService(provOf("openai"), hostOf("openai"), modelOf("gpt-4o"), bindOf("gpt-4o", "openai"), rlOf("rl1", 10), rlOf("org-rl", 20), p)
	svc.Visible = func(kind, id string, _ meta.Owner) bool { return kind != "rate-limit" || id != "rl:org-rl" }
	ref := policyRef("p", OwnerRef{Kind: "user"})
	viewed(t, "rate limits tab", ref, PolicyRateLimitsView{
		RateLimits: []PolicyRateLimitRow{
			{ID: "rl:rl1", Name: "rl1", Default: true, Limits: []Limit{limit(10)}},
			{ID: "rl:org-rl", Hidden: true, Models: []string{"openai/gpt-4o"}, Limits: []Limit{}},
		},
		Unthrottled: []UnthrottledModel{},
	})(svc.PolicyRateLimits(ctx, "p"))
	viewed(t, "model policies", modelRef("gpt-4o"), []ModelPolicyRow{
		{ID: "policy:p", Name: "p", Owner: OwnerRef{Kind: "user"}, Limits: []Limit{limit(10)}, HiddenRateLimits: []string{"rl:org-rl"}},
	})(svc.ModelPolicies(ctx, "gpt-4o"))
	openai := ProviderRef{ID: "prov:openai", Name: "openai"}
	viewed(t, "policy models", ref, []PolicyBindingRow{{
		Provider: openai, Host: hostRef("openai"), Model: modelRef("gpt-4o"), Binding: bindView("gpt-4o", "openai"), MatchedBy: []string{"*"},
		Limits: []Limit{limit(10)}, HiddenRateLimits: []string{"rl:org-rl"},
	}})(svc.PolicyModels(ctx, "p"))
}
