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

func TestPolicyModels_GrantAndLimits(t *testing.T) {
	svc, modID := fixture()
	p, rows, err := svc.PolicyModels(context.Background(), "tier-1")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "tier-1" || p.Owner.Kind != "user" {
		t.Fatalf("policy = %+v", p)
	}
	if len(rows) != 1 || rows[0].Model.ID != modID {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Host.Name != "openai" || rows[0].Provider.Name != "openai" {
		t.Errorf("provider/host = %+v / %+v", rows[0].Provider, rows[0].Host)
	}
	if len(rows[0].MatchedBy) != 1 || rows[0].MatchedBy[0] != "openai/gpt-4o" {
		t.Errorf("matchedBy = %+v", rows[0].MatchedBy)
	}
	if len(rows[0].Limits) != 1 || rows[0].Limits[0].Amount != 100 {
		t.Errorf("limits = %+v", rows[0].Limits)
	}
}

func TestPolicyRateLimits_FlatDefault(t *testing.T) {
	svc, _ := fixture()
	_, view, err := svc.PolicyRateLimits(context.Background(), "tier-1")
	if err != nil {
		t.Fatal(err)
	}
	rows := view.RateLimits
	if len(rows) != 1 || rows[0].Name != "rpm" || !rows[0].Default {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestPolicyHosts_KeysFoldedIn(t *testing.T) {
	provID, hostA, hostB := meta.NewID(), meta.NewID(), meta.NewID()
	modID, keyA := meta.NewID(), meta.NewID()
	svc := &Service{
		Providers: fProviders{{Meta: meta.Metadata{ID: provID, Name: "openai"}}},
		Hosts: fHosts{
			{Meta: meta.Metadata{ID: hostA, Name: "host-a"}},
			{Meta: meta.Metadata{ID: hostB, Name: "host-b"}},
		},
		Models:     fModels{{Meta: meta.Metadata{ID: modID, Name: "gpt-4o", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}}}},
		Bindings:   fBindings{},
		Pricings:   fPricings{},
		HostKeys:   fHostKeys{{Meta: meta.Metadata{ID: keyA, Name: "ka"}, Spec: hostkey.Spec{HostID: hostA}}},
		Policies:   fPolicies{{Meta: meta.Metadata{ID: meta.NewID(), Name: "cust", Owner: meta.Owner{Kind: meta.OwnerUser}}, Spec: policy.Spec{HostKeyIDs: []string{keyA}}}},
		RateLimits: fRLs{},
	}
	_, rows, err := svc.PolicyHosts(context.Background(), "cust")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Host.Name != "host-a" {
		t.Fatalf("rows = %+v", rows)
	}
	if len(rows[0].HostKeys) != 1 || rows[0].HostKeys[0].Name != "ka" {
		t.Errorf("hostKeys = %+v", rows[0].HostKeys)
	}
}

func TestPolicy_NotFound(t *testing.T) {
	svc, _ := fixture()
	if _, _, err := svc.PolicyModels(context.Background(), "nope"); err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestPolicyModelExclusions_HostTierNoBinding(t *testing.T) {
	provID, hostA, hostB := meta.NewID(), meta.NewID(), meta.NewID()
	modID := meta.NewID()
	svc := &Service{
		Providers: fProviders{{Meta: meta.Metadata{ID: provID, Name: "openai"}}},
		Hosts: fHosts{
			{Meta: meta.Metadata{ID: hostA, Name: "host-a"}},
			{Meta: meta.Metadata{ID: hostB, Name: "host-b"}},
		},
		Models: fModels{{Meta: meta.Metadata{ID: modID, Name: "gpt-4o", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}}}},
		// model bound to host-a only; the policy owns host-b
		Bindings:   fBindings{{Meta: meta.Metadata{ID: meta.NewID(), Name: "b"}, Spec: binding.Spec{ModelID: modID, HostID: hostA, Adapter: adapters.OpenAI}}},
		Pricings:   fPricings{},
		HostKeys:   fHostKeys{},
		Policies:   fPolicies{{Meta: meta.Metadata{ID: meta.NewID(), Name: "ht-b", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostB}}, Spec: policy.Spec{}}},
		RateLimits: fRLs{},
	}
	_, granted, err := svc.PolicyModels(context.Background(), "ht-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(granted) != 0 {
		t.Fatalf("granted = %+v, want none", granted)
	}
	_, excl, err := svc.PolicyModelExclusions(context.Background(), "ht-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(excl) != 1 || excl[0].Reason != "host-tier policy's host has no binding for this model" {
		t.Fatalf("exclusions = %+v", excl)
	}
}

func TestHostKeyList_SecretFreeAndScoped(t *testing.T) {
	hostA, hostB := meta.NewID(), meta.NewID()
	svc := &Service{
		Providers: fProviders{},
		Hosts: fHosts{
			{Meta: meta.Metadata{ID: hostA, Name: "host-a"}},
			{Meta: meta.Metadata{ID: hostB, Name: "host-b"}},
		},
		Models:   fModels{},
		Bindings: fBindings{},
		Pricings: fPricings{},
		HostKeys: fHostKeys{
			{Meta: meta.Metadata{ID: meta.NewID(), Name: "ka"}, Spec: hostkey.Spec{HostID: hostA, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "OPENAI_KEY"}}},
			{Meta: meta.Metadata{ID: meta.NewID(), Name: "kb"}, Spec: hostkey.Spec{HostID: hostB, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}}},
		},
		Policies:   fPolicies{},
		RateLimits: fRLs{},
	}
	_, keys, err := svc.HostKeyList(context.Background(), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Name != "ka" || keys[0].Kind != "env" {
		t.Fatalf("keys = %+v", keys)
	}
}

func TestHostPolicies_OwnTierOnly(t *testing.T) {
	hostA := meta.NewID()
	rlID := meta.NewID()
	svc := &Service{
		Providers:  fProviders{},
		Hosts:      fHosts{{Meta: meta.Metadata{ID: hostA, Name: "host-a"}}},
		Models:     fModels{},
		Bindings:   fBindings{},
		Pricings:   fPricings{},
		HostKeys:   fHostKeys{},
		RateLimits: fRLs{{Meta: meta.Metadata{ID: rlID, Name: "rpm"}, Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{Meter: ratelimit.MeterRequests, Amount: 50, Window: time.Minute, Strategy: ratelimit.StrategyTokenBucket}}}}},
		Policies: fPolicies{
			{Meta: meta.Metadata{ID: meta.NewID(), Name: "ht", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostA}}, Spec: policy.Spec{RateLimitID: rlID}},
			{Meta: meta.Metadata{ID: meta.NewID(), Name: "cust", Owner: meta.Owner{Kind: meta.OwnerUser}}, Spec: policy.Spec{}},
		},
	}
	_, rows, err := svc.HostPolicies(context.Background(), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "ht" {
		t.Fatalf("rows = %+v", rows)
	}
	if len(rows[0].RateLimits) != 1 || !rows[0].RateLimits[0].Default || rows[0].RateLimits[0].Name != "rpm" {
		t.Errorf("rateLimits = %+v", rows[0].RateLimits)
	}
}

func TestPolicyModels_DSLMatchedByAndCaps(t *testing.T) {
	provID, hostID, modID := meta.NewID(), meta.NewID(), meta.NewID()
	keyID := meta.NewID()
	svc := &Service{
		Providers: fProviders{{Meta: meta.Metadata{ID: provID, Name: "openai"}}},
		Hosts:     fHosts{{Meta: meta.Metadata{ID: hostID, Name: "openai"}}},
		Models: fModels{{Meta: meta.Metadata{ID: modID, Name: "gpt-4o", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
			Spec: model.Spec{Capabilities: model.Capabilities{Vision: true, Tools: true}, ContextWindowTotal: 128000}}},
		Bindings: fBindings{{Meta: meta.Metadata{ID: meta.NewID(), Name: "b"}, Spec: binding.Spec{ModelID: modID, HostID: hostID, Adapter: adapters.OpenAI}}},
		Pricings: fPricings{},
		HostKeys: fHostKeys{{Meta: meta.Metadata{ID: keyID, Name: "k"}, Spec: hostkey.Spec{HostID: hostID}}},
		// customer policy, DSL grant "openai/*", key reaches the host
		Policies:   fPolicies{{Meta: meta.Metadata{ID: meta.NewID(), Name: "dsl", Owner: meta.Owner{Kind: meta.OwnerUser}}, Spec: policy.Spec{Models: []string{"openai"}, HostKeyIDs: []string{keyID}}}},
		RateLimits: fRLs{},
	}
	_, rows, err := svc.PolicyModels(context.Background(), "dsl")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	if len(rows[0].MatchedBy) != 1 || rows[0].MatchedBy[0] != "openai" {
		t.Errorf("matchedBy = %+v, want raw stored ref", rows[0].MatchedBy)
	}
	caps := rows[0].Model.Capabilities
	if len(caps) != 2 || rows[0].Model.ContextWindowTotal != 128000 {
		t.Errorf("model caps/context = %+v / %d", caps, rows[0].Model.ContextWindowTotal)
	}
}

func TestPolicyHosts_KeyEnrichmentAndRequirement(t *testing.T) {
	provID, hostA, hostB := meta.NewID(), meta.NewID(), meta.NewID()
	modSolo, modDual := meta.NewID(), meta.NewID()
	keyA, keyB := meta.NewID(), meta.NewID()
	disabled := false
	svc := &Service{
		Providers: fProviders{{Meta: meta.Metadata{ID: provID, Name: "openai"}}},
		Hosts: fHosts{
			{Meta: meta.Metadata{ID: hostA, Name: "host-a"}},
			{Meta: meta.Metadata{ID: hostB, Name: "host-b"}},
		},
		Models: fModels{
			{Meta: meta.Metadata{ID: modSolo, Name: "solo", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}}},
			{Meta: meta.Metadata{ID: modDual, Name: "dual", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}}},
		},
		// solo served only on host-a; dual served on both
		Bindings: fBindings{
			{Meta: meta.Metadata{ID: meta.NewID(), Name: "b1"}, Spec: binding.Spec{ModelID: modSolo, HostID: hostA, Adapter: adapters.OpenAI}},
			{Meta: meta.Metadata{ID: meta.NewID(), Name: "b2"}, Spec: binding.Spec{ModelID: modDual, HostID: hostA, Adapter: adapters.OpenAI}},
			{Meta: meta.Metadata{ID: meta.NewID(), Name: "b3"}, Spec: binding.Spec{ModelID: modDual, HostID: hostB, Adapter: adapters.OpenAI}},
		},
		Pricings: fPricings{},
		HostKeys: fHostKeys{
			{Meta: meta.Metadata{ID: keyA, Name: "ka"}, Spec: hostkey.Spec{HostID: hostA, Enabled: &disabled}},
			{Meta: meta.Metadata{ID: keyB, Name: "kb"}, Spec: hostkey.Spec{HostID: hostB}},
		},
		Policies: fPolicies{
			{Meta: meta.Metadata{ID: meta.NewID(), Name: "p1", Owner: meta.Owner{Kind: meta.OwnerUser}}, Spec: policy.Spec{HostKeyIDs: []string{keyA, keyB}}},
			// a second policy that also uses keyA → sharedWithPolicyCount = 1
			{Meta: meta.Metadata{ID: meta.NewID(), Name: "p2", Owner: meta.Owner{Kind: meta.OwnerUser}}, Spec: policy.Spec{HostKeyIDs: []string{keyA}}},
		},
		RateLimits: fRLs{},
	}
	_, rows, err := svc.PolicyHosts(context.Background(), "p1")
	if err != nil {
		t.Fatal(err)
	}
	byHost := map[string]PolicyHostRow{}
	for _, r := range rows {
		byHost[r.Host.Name] = r
	}
	a, b := byHost["host-a"], byHost["host-b"]
	if a.Requirement != "required" { // solo is only on host-a
		t.Errorf("host-a requirement = %q, want required", a.Requirement)
	}
	if b.Requirement != "optional" { // dual is also on host-a
		t.Errorf("host-b requirement = %q, want optional", b.Requirement)
	}
	if len(a.HostKeys) != 1 || a.HostKeys[0].Enabled || a.HostKeys[0].SharedWithPolicyCount != 1 {
		t.Errorf("host-a key = %+v (want disabled, shared=1)", a.HostKeys)
	}
}

func TestPolicyRateLimits_OverlapsAndUnthrottled(t *testing.T) {
	provID, hostID := meta.NewID(), meta.NewID()
	modA, modFree := meta.NewID(), meta.NewID()
	keyID := meta.NewID()
	rl1, rl2 := meta.NewID(), meta.NewID()
	svc := &Service{
		Providers: fProviders{{Meta: meta.Metadata{ID: provID, Name: "openai"}}},
		Hosts:     fHosts{{Meta: meta.Metadata{ID: hostID, Name: "openai"}}},
		Models: fModels{
			{Meta: meta.Metadata{ID: modA, Name: "gpt-4o", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}}},
			{Meta: meta.Metadata{ID: modFree, Name: "free-model", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}}},
		},
		Bindings: fBindings{
			{Meta: meta.Metadata{ID: meta.NewID(), Name: "b1"}, Spec: binding.Spec{ModelID: modA, HostID: hostID, Adapter: adapters.OpenAI}},
			{Meta: meta.Metadata{ID: meta.NewID(), Name: "b2"}, Spec: binding.Spec{ModelID: modFree, HostID: hostID, Adapter: adapters.OpenAI}},
		},
		Pricings: fPricings{},
		HostKeys: fHostKeys{{Meta: meta.Metadata{ID: keyID, Name: "k"}, Spec: hostkey.Spec{HostID: hostID}}},
		RateLimits: fRLs{
			{Meta: meta.Metadata{ID: rl1, Name: "rl1"}, Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{Meter: ratelimit.MeterRequests, Amount: 10, Window: time.Minute, Strategy: ratelimit.StrategyTokenBucket}}}},
			{Meta: meta.Metadata{ID: rl2, Name: "rl2"}, Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{Meter: ratelimit.MeterRequests, Amount: 20, Window: time.Minute, Strategy: ratelimit.StrategyTokenBucket}}}},
		},
		// two RLBindings both matching gpt-4o (overlap); free-model matched by neither (unthrottled)
		Policies: fPolicies{{Meta: meta.Metadata{ID: meta.NewID(), Name: "p", Owner: meta.Owner{Kind: meta.OwnerUser}}, Spec: policy.Spec{
			HostKeyIDs: []string{keyID},
			RLBindings: []policy.RLBinding{
				{Models: []string{"openai/gpt-4o"}, RateLimitID: rl1},
				{Models: []string{"openai"}, RateLimitID: rl2},
			},
		}}},
	}
	_, view, err := svc.PolicyRateLimits(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Overlaps) != 1 {
		t.Fatalf("overlaps = %+v", view.Overlaps)
	}
	o := view.Overlaps[0]
	if o.Model != "gpt-4o" || o.Winner != rl1 || len(o.Losers) != 1 || o.Losers[0] != rl2 {
		t.Errorf("overlap = %+v", o)
	}
	// free-model is matched by the "openai" RLBinding too, so it's capped — NOT unthrottled.
	for _, u := range view.Unthrottled {
		if u.Model.Name == "gpt-4o" {
			t.Errorf("gpt-4o should be capped, not unthrottled")
		}
	}
}
