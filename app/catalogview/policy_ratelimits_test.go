package catalogview

import (
	"context"
	"testing"
)

func TestPolicyRateLimits(t *testing.T) {
	ctx := context.Background()
	viewed(t, "one unscoped link", policyRef("tier-1", OwnerRef{Kind: "user"}), PolicyRateLimitsView{
		RateLimits:  []PolicyRateLimitRow{{ID: "rl:rpm", Name: "rpm", Default: true, Limits: []Limit{limit(100)}}},
		Unthrottled: []UnthrottledModel{},
	})(tier1().PolicyRateLimits(ctx, "tier-1"))

	// Every link whose scope matches applies: gpt-4o gets both caps, and the
	// models no link covers on any host are reported unthrottled. half is
	// capped on azure only, which is enough to count as throttled.
	p := limitedBy(limitedBy(limitedBy(policyOf("p", operator, nil), "rl1", "openai/gpt-4o"), "rl2", "openai/gpt-4o"), "rl3", "openai/half@azure")
	svc := newService(
		provOf("openai"), hostOf("openai"), hostOf("azure"), modelOf("gpt-4o"), modelOf("z-free"), modelOf("a-free"), modelOf("half"),
		bindOf("gpt-4o", "openai"), bindOf("z-free", "openai"), bindOf("a-free", "openai"), bindOf("half", "openai"), bindOf("half", "azure"),
		rlOf("rl1", 10), rlOf("rl2", 20), rlOf("rl3", 30), p,
	)
	ref := policyRef("p", OwnerRef{Kind: "user"})
	viewed(t, "scoped links", ref, PolicyRateLimitsView{
		RateLimits: []PolicyRateLimitRow{
			{ID: "rl:rl1", Name: "rl1", Models: []string{"openai/gpt-4o"}, Limits: []Limit{limit(10)}},
			{ID: "rl:rl2", Name: "rl2", Models: []string{"openai/gpt-4o"}, Limits: []Limit{limit(20)}},
			{ID: "rl:rl3", Name: "rl3", Models: []string{"openai/half@azure"}, Limits: []Limit{limit(30)}},
		},
		Unthrottled: []UnthrottledModel{{Model: modelRef("a-free")}, {Model: modelRef("z-free")}},
	})(svc.PolicyRateLimits(ctx, "p"))

	openai := ProviderRef{ID: "prov:openai", Name: "openai"}
	row := func(modelName, hostName string, limits ...Limit) PolicyBindingRow {
		return PolicyBindingRow{Provider: openai, Host: hostRef(hostName), Model: modelRef(modelName), Binding: bindView(modelName, hostName), MatchedBy: []string{"*"}, Limits: append([]Limit{}, limits...)}
	}
	viewed(t, "limits per binding", ref, []PolicyBindingRow{
		row("a-free", "openai"),
		row("gpt-4o", "openai", limit(10), limit(20)),
		row("half", "azure", limit(30)),
		row("half", "openai"),
		row("z-free", "openai"),
	})(svc.PolicyModels(ctx, "p"))
}
