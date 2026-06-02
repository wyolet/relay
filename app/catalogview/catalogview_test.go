package catalogview

import (
	"context"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
)

type (
	fModels    []*model.Model
	fHosts     []*host.Host
	fBindings  []*binding.Binding
	fPricings  []*pricing.Pricing
	fPolicies  []*policy.Policy
	fRLs       []*ratelimit.RateLimit
	fProviders []*provider.Provider
	fHostKeys  []*hostkey.HostKey
)

func (f fHostKeys) List(context.Context) ([]*hostkey.HostKey, error) { return f, nil }

func (f fModels) List(context.Context) ([]*model.Model, error)          { return f, nil }
func (f fHosts) List(context.Context) ([]*host.Host, error)             { return f, nil }
func (f fBindings) List(context.Context) ([]*binding.Binding, error)    { return f, nil }
func (f fPricings) List(context.Context) ([]*pricing.Pricing, error)    { return f, nil }
func (f fPolicies) List(context.Context) ([]*policy.Policy, error)      { return f, nil }
func (f fRLs) List(context.Context) ([]*ratelimit.RateLimit, error)     { return f, nil }
func (f fProviders) List(context.Context) ([]*provider.Provider, error) { return f, nil }

func fixture() (*Service, string) {
	provID, hostID, modID, rlID := meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID()
	pricingID := meta.NewID()

	svc := &Service{
		Providers:  fProviders{{Meta: meta.Metadata{ID: provID, Name: "openai"}}},
		Hosts:      fHosts{{Meta: meta.Metadata{ID: hostID, Name: "openai", DisplayName: "OpenAI"}, Spec: host.Spec{BaseURL: "https://api.openai.com"}}},
		Models:     fModels{{Meta: meta.Metadata{ID: modID, Name: "gpt-4o", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}}}},
		Bindings:   fBindings{{Meta: meta.Metadata{ID: meta.NewID(), Name: "gpt-4o-on-openai"}, Spec: binding.Spec{ModelID: modID, HostID: hostID, Adapter: adapters.OpenAI, PricingID: pricingID}}},
		Pricings:   fPricings{{Meta: meta.Metadata{ID: pricingID, Name: "openai-gpt-4o", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}}, Spec: pricing.Spec{Currency: "USD", TargetModelIDs: []string{modID}, Rates: []pricing.Rate{{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: 2.5}}}}},
		Policies:   fPolicies{{Meta: meta.Metadata{ID: meta.NewID(), Name: "tier-1", Owner: meta.Owner{Kind: meta.OwnerUser}}, Spec: policy.Spec{ModelIDs: []string{modID}, RateLimitID: rlID}}},
		RateLimits: fRLs{{Meta: meta.Metadata{ID: rlID, Name: "rpm"}, Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{Meter: ratelimit.MeterRequests, Amount: 100, Window: time.Minute, Strategy: ratelimit.StrategyTokenBucket}}}}},
		HostKeys:   fHostKeys{},
	}
	return svc, modID
}

func TestModelHosts(t *testing.T) {
	svc, modID := fixture()
	m, rows, err := svc.ModelHosts(context.Background(), "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != modID || len(rows) != 1 {
		t.Fatalf("model=%s rows=%d, want %s/1", m.ID, len(rows), modID)
	}
	r := rows[0]
	if r.Host.Name != "openai" || r.Binding.Adapter != "openai" {
		t.Errorf("row = %+v", r)
	}
	if r.Pricing == nil || len(r.Pricing.Rates) != 1 || r.Pricing.Rates[0].Amount != 2.5 {
		t.Errorf("pricing = %+v", r.Pricing)
	}
}

func TestModelPricing_FlatHostInline(t *testing.T) {
	svc, _ := fixture()
	_, rows, err := svc.ModelPricing(context.Background(), "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Host.Name != "openai" || rows[0].Currency != "USD" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestModelPolicies_GrantAndLimits(t *testing.T) {
	svc, _ := fixture()
	_, rows, err := svc.ModelPolicies(context.Background(), "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("policies = %d, want 1", len(rows))
	}
	p := rows[0]
	if p.Name != "tier-1" || p.Owner.Kind != "user" {
		t.Errorf("policy = %+v", p)
	}
	if len(p.Limits) != 1 || p.Limits[0].Meter != "requests" || p.Limits[0].Amount != 100 {
		t.Errorf("limits = %+v", p.Limits)
	}
}

// TestModelPolicies_Filtering is the regression for the bug where every
// wildcard policy leaked into every model. A wildcard customer policy whose
// keys don't reach the model's host must be EXCLUDED; a host-tier wildcard
// policy on the serving host must be INCLUDED.
func TestModelPolicies_Filtering(t *testing.T) {
	provID := meta.NewID()
	hostA, hostB := meta.NewID(), meta.NewID() // model served only on hostA
	modID := meta.NewID()
	keyB := meta.NewID() // a key on hostB (does NOT reach the model)

	svc := &Service{
		Providers: fProviders{{Meta: meta.Metadata{ID: provID, Name: "openai"}}},
		Hosts: fHosts{
			{Meta: meta.Metadata{ID: hostA, Name: "openai"}, Spec: host.Spec{BaseURL: "http://a"}},
			{Meta: meta.Metadata{ID: hostB, Name: "azure"}, Spec: host.Spec{BaseURL: "http://b"}},
		},
		Models:   fModels{{Meta: meta.Metadata{ID: modID, Name: "gpt-4o", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}}}},
		Bindings: fBindings{{Meta: meta.Metadata{ID: meta.NewID(), Name: "b"}, Spec: binding.Spec{ModelID: modID, HostID: hostA, Adapter: adapters.OpenAI}}},
		Pricings: fPricings{},
		HostKeys: fHostKeys{{Meta: meta.Metadata{ID: keyB, Name: "kb"}, Spec: hostkey.Spec{HostID: hostB}}},
		Policies: fPolicies{
			// wildcard customer policy whose only key is on hostB → cannot reach the model → EXCLUDE
			{Meta: meta.Metadata{ID: meta.NewID(), Name: "unreachable-wildcard", Owner: meta.Owner{Kind: meta.OwnerUser}}, Spec: policy.Spec{HostKeyIDs: []string{keyB}}},
			// host-tier wildcard on hostA (serves the model) → INCLUDE
			{Meta: meta.Metadata{ID: meta.NewID(), Name: "hostA-tier", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostA}}, Spec: policy.Spec{}},
			// host-tier wildcard on hostB (does NOT serve the model) → EXCLUDE
			{Meta: meta.Metadata{ID: meta.NewID(), Name: "hostB-tier", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostB}}, Spec: policy.Spec{}},
		},
		RateLimits: fRLs{},
	}

	_, rows, err := svc.ModelPolicies(context.Background(), "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Name] = true
	}
	if !got["hostA-tier"] {
		t.Error("hostA-tier (serves the model) should be included")
	}
	if got["unreachable-wildcard"] {
		t.Error("unreachable-wildcard (no key reaches the model's host) must be excluded")
	}
	if got["hostB-tier"] {
		t.Error("hostB-tier (host does not serve the model) must be excluded")
	}
}

func TestModel_NotFound(t *testing.T) {
	svc, _ := fixture()
	if _, _, err := svc.ModelHosts(context.Background(), "nope"); err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}
