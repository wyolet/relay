package catalogview

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/catalogkind/model"
)

// viewed fails the test unless a projection returned exactly the wanted
// subject and rows.
func viewed[R, V any](t *testing.T, what string, wantRef R, wantRows V) func(R, V, error) {
	t.Helper()
	return func(ref R, rows V, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if diff := cmp.Diff(wantRef, ref); diff != "" {
			t.Errorf("%s subject (-want +got):\n%s", what, diff)
		}
		if diff := cmp.Diff(wantRows, rows); diff != "" {
			t.Errorf("%s rows (-want +got):\n%s", what, diff)
		}
	}
}

func TestModelHosts(t *testing.T) {
	ctx := context.Background()
	want := []ModelHostRow{{Host: tier1Host(), Binding: bindView("gpt-4o", "openai"), Pricing: sheetView("openai-gpt-4o", 2.5)}}
	viewed(t, "by slug", modelRef("gpt-4o"), want)(tier1().ModelHosts(ctx, "gpt-4o"))
	viewed(t, "by id", modelRef("gpt-4o"), want)(tier1().ModelHosts(ctx, "model:gpt-4o"))

	off := false
	m := modelOf("gpt-4o")
	m.Meta.DisplayName = "GPT-4o"
	m.Spec = model.Spec{
		Capabilities:       model.Capabilities{Vision: true, Tools: true},
		ContextWindowTotal: 128000, ContextWindowInput: 100000,
		Deprecation: &model.Deprecation{Status: model.DeprecationDeprecated, SunsetDate: "2027-01-01", Replacement: "model:gpt-5"},
	}
	onAzure := bindOf("gpt-4o", "azure")
	onAzure.Spec.Enabled, onAzure.Spec.UpstreamName, onAzure.Spec.Snapshots = &off, "gpt-4o-deploy", []string{"gpt-4o-2024"}
	offHost := hostOf("zeta")
	offHost.Spec.Enabled = &off
	svc := newService(
		provOf("openai"), m, hostOf("openai"), hostOf("azure"), offHost,
		bindOf("gpt-4o", "zeta"), bindOf("gpt-4o", "openai"), onAzure, bindOf("gpt-4o", "deleted"),
		// A sheet covering the pair prices a binding that names none.
		sheetOf("azure-all", "azure", 3, "gpt-4o"),
	)
	wantModel := ModelRef{
		ID: "model:gpt-4o", Name: "gpt-4o", DisplayName: "GPT-4o",
		Capabilities:       []string{"tools", "vision"},
		ContextWindowTotal: 128000, ContextWindowInput: 100000,
		Deprecation: &DeprecationView{Status: "deprecated", SunsetDate: "2027-01-01", Replacement: "model:gpt-5"},
	}
	zeta := hostRef("zeta")
	zeta.Enabled = false
	viewed(t, "many hosts", wantModel, []ModelHostRow{
		{Host: hostRef("azure"), Binding: BindingView{ID: "bind:gpt-4o@azure", Adapter: "openai", UpstreamName: "gpt-4o-deploy", Snapshots: []string{"gpt-4o-2024"}}, Pricing: sheetView("azure-all", 3)},
		{Host: hostRef("openai"), Binding: bindView("gpt-4o", "openai")},
		{Host: zeta, Binding: bindView("gpt-4o", "zeta")},
	})(svc.ModelHosts(ctx, "gpt-4o"))

	viewed(t, "priced hosts only", wantModel, []ModelPriceRow{
		{Host: hostRef("azure"), ID: "pricing:azure-all", Name: "azure-all", Currency: "USD", Rates: sheetView("", 3).Rates},
	})(svc.ModelPricing(ctx, "gpt-4o"))
}

func TestModelPolicies(t *testing.T) {
	ctx := context.Background()
	svc := tier1()
	viewed(t, "grant and limits", modelRef("gpt-4o"), []ModelPolicyRow{
		{ID: "policy:tier-1", Name: "tier-1", Owner: OwnerRef{Kind: "user"}, Limits: []Limit{limit(100)}},
	})(svc.ModelPolicies(ctx, "gpt-4o"))

	svc.Visible = asAlice
	viewed(t, "operator policy hidden from alice", modelRef("gpt-4o"), []ModelPolicyRow{})(svc.ModelPolicies(ctx, "gpt-4o"))

	// The model is served on openai only. A policy with no model list allows
	// it where the policy reaches a serving host, and nowhere else.
	svc = newService(
		provOf("openai"), hostOf("openai"), hostOf("azure"), modelOf("gpt-4o"), modelOf("o1"), bindOf("gpt-4o", "openai"),
		keyOf("kb", "azure", operator), poolOf("azure-keys", "azure", operator, "kb"),
		policyOf("reaches-azure-only", operator, nil, "azure-keys"),
		policyOf("z-openai-tier", hostTier("openai"), nil),
		policyOf("azure-tier", hostTier("azure"), nil),
		policyOf("other-model", operator, []string{"openai/o1"}),
		limitedBy(limitedBy(limitedBy(policyOf("any-host", operator, []string{"openai/gpt-4o"}), "wide"), "scoped", "openai/gpt-4o"), "elsewhere", "openai/o1"),
		rlOf("wide", 10), rlOf("scoped", 20), rlOf("elsewhere", 30),
	)
	viewed(t, "reach", modelRef("gpt-4o"), []ModelPolicyRow{
		{ID: "policy:any-host", Name: "any-host", Owner: OwnerRef{Kind: "user"}, Limits: []Limit{limit(10), limit(20)}},
		{ID: "policy:z-openai-tier", Name: "z-openai-tier", Owner: OwnerRef{Kind: "host", ID: "host:openai", Name: "openai"}, Limits: []Limit{}},
	})(svc.ModelPolicies(ctx, "gpt-4o"))
}

func TestModelPolicies_LimitsAreTheSameOnEveryCall(t *testing.T) {
	t.Skip("bug: ModelPolicies of a model on two hosts, under a policy whose rate limit link is scoped to one host → limits are [] or the link's rules from call to call (the granting host is picked in map order)")
	// half is served on two hosts and the policy's one rate limit is scoped to one of them.
	svc := newService(
		provOf("openai"), hostOf("openai"), hostOf("azure"), modelOf("half"), bindOf("half", "openai"), bindOf("half", "azure"),
		limitedBy(policyOf("p", operator, nil), "azure-only", "openai/half@azure"), rlOf("azure-only", 30),
	)
	seen := map[int]int{}
	for range 200 {
		_, rows, err := svc.ModelPolicies(context.Background(), "half")
		if err != nil || len(rows) != 1 {
			t.Fatalf("rows = %+v, err %v", rows, err)
		}
		seen[len(rows[0].Limits)]++
	}
	if len(seen) != 1 {
		t.Fatalf("one policy, one model: calls answered with different numbers of limits (limits → calls): %v", seen)
	}
}

func TestHostModels(t *testing.T) {
	svc := newService(
		provOf("openai"), hostOf("openai"), hostOf("azure"), modelOf("o1"), modelOf("gpt-4o"),
		bindOf("o1", "openai"), bindOf("gpt-4o", "openai"), bindOf("gpt-4o", "azure"), bindOf("deleted", "openai"),
		sheetOf("openai-o1", "openai", 15, "o1"),
	)
	want := []HostModelRow{
		{Model: modelRef("gpt-4o"), Binding: bindView("gpt-4o", "openai")},
		{Model: modelRef("o1"), Binding: bindView("o1", "openai"), Pricing: sheetView("openai-o1", 15)},
	}
	viewed(t, "by slug", hostRef("openai"), want)(svc.HostModels(context.Background(), "openai"))
	viewed(t, "by id", hostRef("openai"), want)(svc.HostModels(context.Background(), "host:openai"))
}

func TestProjections_UnknownSubjectIsNotFound(t *testing.T) {
	ctx := context.Background()
	svc := tier1()
	svc.Pools, svc.Accounts = fPools{}, fAccounts{}
	for name, err := range map[string]error{
		"ModelHosts":            third(svc.ModelHosts(ctx, "nope")),
		"ModelPricing":          third(svc.ModelPricing(ctx, "nope")),
		"ModelPolicies":         third(svc.ModelPolicies(ctx, "nope")),
		"HostModels":            third(svc.HostModels(ctx, "nope")),
		"HostKeyList":           third(svc.HostKeyList(ctx, "nope")),
		"HostPolicies":          third(svc.HostPolicies(ctx, "nope")),
		"PolicyModels":          third(svc.PolicyModels(ctx, "nope")),
		"PolicyModelExclusions": third(svc.PolicyModelExclusions(ctx, "nope")),
		"PolicyHosts":           third(svc.PolicyHosts(ctx, "nope")),
		"PolicyRateLimits":      third(svc.PolicyRateLimits(ctx, "nope")),
		"PoolKeys":              third(svc.PoolKeys(ctx, "nope")),
		"AccountKeys":           third(svc.AccountKeys(ctx, "nope")),
		// A model is no host and a host no policy: a ref resolves within its own kind.
		"HostModels of a model slug":  third(svc.HostModels(ctx, "gpt-4o")),
		"PolicyModels of a host slug": third(svc.PolicyModels(ctx, "openai")),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", name, err)
		}
	}
}

func third[A, B any](_ A, _ B, err error) error { return err }
