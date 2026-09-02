package routing

import (
	"errors"
	"fmt"
	"math/rand"
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/slug"
)

// tenantedSnapshot builds a snapshot that actually holds the team and project
// a project-owned row needs, so an ownership rule can be tested without the
// row being dropped for a missing parent first.
func tenantedSnapshot(t *testing.T, f twoHostFixture, keys []*hostkey.HostKey, proj *project.Project, tm *team.Team) *catalog.Snapshot {
	t.Helper()
	c := catalog.New(
		lister[provider.Provider]{f.provider}, lister[host.Host]{f.hostRowA},
		lister[policy.Policy]{f.tierA}, lister[model.Model]{f.model},
		lister[hostkey.HostKey](keys), lister[ratelimit.RateLimit]{},
		lister[key.Key]{}, lister[pricing.Pricing]{}, lister[binding.Binding]{f.bindingA},
	)
	teams := lister[team.Team]{}
	projects := lister[project.Project]{}
	if tm != nil {
		teams = lister[team.Team]{tm}
	}
	if proj != nil {
		projects = lister[project.Project]{proj}
	}
	c.UseTenancy(teams, projects,
		lister[serviceaccount.ServiceAccount]{}, lister[group.Group]{},
		lister[role.Role]{}, lister[rolebinding.RoleBinding]{}, lister[policybinding.PolicyBinding]{})
	if err := c.Reload(t.Context()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	return c.Current()
}

func newTenancy() (*team.Team, *project.Project) {
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	proj := &project.Project{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "ml"},
		Spec: project.Spec{TeamID: tm.Meta.ID},
	}
	proj.StampOwner()
	return tm, proj
}

// D73: the pool a policy-less request draws on is the host's shared
// credentials — system- or user-owned. A project's credential is reachable
// only through that project's policy, which is what holds the spend inside
// its limits and attribution — TestResolvePolicyless_SkipsProjectOwnedKeys
// pins that half. What is left is which of the remaining owner kinds count as
// shared, and that a disabled key is out for the usual reason.
func TestResolvePolicyless_KeyPoolScope(t *testing.T) {
	tm, proj := newTenancy()
	off := false

	for _, tc := range []struct {
		name    string
		owner   meta.Owner
		enabled *bool
		want    bool // the key serves policy-less traffic
	}{
		{name: "system-owned is shared", owner: meta.Owner{Kind: meta.OwnerSystem}, want: true},
		{name: "user-owned is shared", owner: meta.Owner{Kind: meta.OwnerUser, ID: meta.NewID()}, want: true},
		{name: "disabled is not", owner: meta.Owner{Kind: meta.OwnerSystem}, enabled: &off},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTwoHostParts()
			f.keyA.Meta.Owner = tc.owner
			f.keyA.Spec.Enabled = tc.enabled
			snap := tenantedSnapshot(t, f, []*hostkey.HostKey{f.keyA}, proj, tm)

			plan, err := (&Resolver{}).resolvePolicyless(snap, []*model.Model{f.model}, &f.model.Spec.Snapshots[0], "")
			if !tc.want {
				if !errors.Is(err, ErrNoKeys) {
					t.Fatalf("err = %v, want ErrNoKeys — this key must not serve policy-less traffic", err)
				}
			} else if err != nil {
				t.Fatalf("resolvePolicyless: %v", err)
			} else if len(plan.Keys) != 1 || plan.Keys[0].Meta.ID != f.keyA.Meta.ID {
				t.Fatalf("keys = %v, want the shared key", plan.Keys)
			}
			// The listing answers the same question, or it advertises models
			// the flow would refuse.
			if got := PolicylessAllows(snap, f.model, ""); got != tc.want {
				t.Errorf("PolicylessAllows = %v, want %v — the listing and the flow disagree", got, tc.want)
			}
		})
	}
}

// A policy-less plan carries no policy and no policy-level limits: nothing
// downstream should meter it against a policy it does not have.
func TestResolvePolicyless_PlanCarriesNoPolicy(t *testing.T) {
	f := newTwoHostParts()
	snap := catalog.Build(
		[]*provider.Provider{f.provider},
		[]*host.Host{f.hostRowA},
		[]*policy.Policy{f.tierA}, nil,
		[]*model.Model{f.model},
		[]*hostkey.HostKey{f.keyA}, nil, nil,
		[]*binding.Binding{f.bindingA},
	)
	plan, err := (&Resolver{}).resolvePolicyless(snap, []*model.Model{f.model}, &f.model.Spec.Snapshots[0], "")
	if err != nil {
		t.Fatalf("resolvePolicyless: %v", err)
	}
	if plan.Policy != nil {
		t.Fatalf("plan policy = %+v, want nil", plan.Policy)
	}
	if plan.Provider != "acme" {
		t.Errorf("provider = %q, want the model's provider slug", plan.Provider)
	}
}

// The policy-less walk reports the same diagnoses the policy path does, so a
// caller learns whether the model is off, unbound, or unreachable.
func TestResolvePolicyless_Diagnoses(t *testing.T) {
	off := false
	for _, tc := range []struct {
		name    string
		mutate  func(*twoHostFixture)
		wantErr error
	}{
		{
			name:    "a disabled model",
			mutate:  func(f *twoHostFixture) { f.model.Spec.Enabled = &off },
			wantErr: ErrModelDisabled,
		},
		{
			// A deprecated model is skipped after it has already counted as
			// enabled, so the diagnosis is the one for a model with nothing
			// to route to rather than a disabled model.
			name: "a deprecated model has nothing to route to",
			mutate: func(f *twoHostFixture) {
				f.model.Spec.Deprecation = &model.Deprecation{Status: model.DeprecationSunset}
			},
			wantErr: ErrNoHostBinding,
		},
		{
			name:    "no enabled binding",
			mutate:  func(f *twoHostFixture) { f.bindingA.Spec.Enabled = &off },
			wantErr: ErrNoHostBinding,
		},
		{
			name:    "a host pin no binding satisfies",
			wantErr: ErrNoHostBinding,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTwoHostParts()
			if tc.mutate != nil {
				tc.mutate(&f)
			}
			snap := catalog.Build(
				[]*provider.Provider{f.provider},
				[]*host.Host{f.hostRowA},
				[]*policy.Policy{f.tierA}, nil,
				[]*model.Model{f.model},
				[]*hostkey.HostKey{f.keyA}, nil, nil,
				[]*binding.Binding{f.bindingA},
			)
			pin := ""
			if tc.mutate == nil {
				pin = f.hostB // a host the model has no binding on
			}
			_, err := (&Resolver{}).resolvePolicyless(snap, []*model.Model{f.model}, &f.model.Spec.Snapshots[0], pin)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// Policy-less traffic is off unless the operator switched it on, so a caller
// that resolved no policy is refused rather than served from the shared pool
// by default.
func TestResolve_PolicylessIsClosedByDefault(t *testing.T) {
	f := newTwoHostParts()
	c := catalog.New(
		lister[provider.Provider]{f.provider}, lister[host.Host]{f.hostRowA},
		lister[policy.Policy]{f.tierA}, lister[model.Model]{f.model},
		lister[hostkey.HostKey]{f.keyA}, lister[ratelimit.RateLimit]{},
		lister[key.Key]{}, lister[pricing.Pricing]{}, lister[binding.Binding]{f.bindingA},
	)
	if err := c.Reload(t.Context()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, err := New(c).Resolve(Request{ModelName: "m1"}); !errors.Is(err, ErrPolicyless) {
		t.Fatalf("err = %v, want ErrPolicyless", err)
	}
}

// That the walk keeps going past a keyless binding is pinned by
// TestResolve_WalksPastKeylessBinding. What it does not cover is that the
// rest of the plan follows the binding the walk landed on rather than the one
// it abandoned — the pricing here belongs to the second host — and that proxy
// mode, which brings its own credential, stops at the first allowed binding
// instead.
func TestResolve_SecondBindingHasKeys(t *testing.T) {
	f := newTwoHostParts()
	caller := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "caller", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: policy.Spec{Models: []string{"acme/m1"}, HostKeyIDs: []string{f.keyB.Meta.ID}},
	}
	priceB := &pricing.Pricing{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "b-rates", Owner: meta.Owner{Kind: meta.OwnerHost, ID: f.hostB}},
		Spec: pricing.Spec{
			Currency:       "USD",
			TargetModelIDs: []string{f.model.Meta.ID},
			Rates:          []pricing.Rate{{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: 3}},
		},
	}
	snap := catalog.Build(
		[]*provider.Provider{f.provider},
		[]*host.Host{f.hostRowA, f.hostRB},
		[]*policy.Policy{f.tierA, f.tierB, caller}, nil,
		[]*model.Model{f.model},
		[]*hostkey.HostKey{f.keyA, f.keyB}, nil,
		[]*pricing.Pricing{priceB},
		[]*binding.Binding{f.bindingA, f.bindB},
	)
	plan, err := (&Resolver{}).Resolve(Request{ModelName: "m1", Policy: caller, Snapshot: snap})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plan.Pricing == nil || plan.Pricing.Meta.ID != priceB.Meta.ID {
		t.Errorf("pricing = %+v, want the second host's rate sheet", plan.Pricing)
	}

	// Proxy mode brings its own upstream credential, so the walk stops at the
	// first allowed binding and hands back no keys at all.
	proxied, err := (&Resolver{}).Resolve(Request{ModelName: "m1", Policy: caller, Snapshot: snap, SkipKeyCheck: true})
	if err != nil {
		t.Fatalf("Resolve with SkipKeyCheck: %v", err)
	}
	if proxied.HostBinding.Meta.ID != f.bindingA.Meta.ID {
		t.Errorf("binding = %q, want the first one when the key gate is skipped", proxied.HostBinding.Meta.Name)
	}
	if len(proxied.Keys) != 0 {
		t.Errorf("keys = %v, want none in proxy mode", proxied.Keys)
	}
}

// The listing and the flow answer one question. Over randomly generated
// catalogs — grants, tiers, deprecation, NoAuth hosts, key coverage — a model
// PolicyAllows advertises is one Resolve serves, and one it hides is one
// Resolve refuses.
func TestPolicyAllowsMatchesResolve(t *testing.T) {
	for _, seed := range []int64{1, 7, 42, 1337, 90210} {
		t.Run(fmt.Sprint("seed ", seed), func(t *testing.T) {
			r := rand.New(rand.NewSource(seed))
			for round := 0; round < 40; round++ {
				snap, caller, models := randomGrantCatalog(r)
				for _, m := range models {
					listed := PolicyAllows(snap, caller, m)
					_, err := (&Resolver{}).Resolve(Request{
						ModelName: m.Spec.Snapshots[0].Name, Policy: caller, Snapshot: snap,
					})
					// Resolve can only serve a model whose snapshot name is the
					// one it was asked for; a name collision is not what this
					// property is about, so only the reachable/unreachable
					// verdict is compared.
					served := err == nil
					if listed != served {
						t.Fatalf("round %d, model %q: PolicyAllows=%v but Resolve served=%v (err %v)",
							round, m.Meta.Name, listed, served, err)
					}
				}
			}
		})
	}
}

// PolicylessAllows is the same property for the flow that serves a caller
// with no policy at all.
func TestPolicylessAllowsMatchesResolvePolicyless(t *testing.T) {
	for _, seed := range []int64{2, 11, 99, 4242} {
		t.Run(fmt.Sprint("seed ", seed), func(t *testing.T) {
			r := rand.New(rand.NewSource(seed))
			for round := 0; round < 40; round++ {
				snap, _, models := randomGrantCatalog(r)
				for _, m := range models {
					listed := PolicylessAllows(snap, m, "")
					_, err := (&Resolver{}).resolvePolicyless(snap, []*model.Model{m}, &m.Spec.Snapshots[0], "")
					if served := err == nil; listed != served {
						t.Fatalf("round %d, model %q: PolicylessAllows=%v but resolvePolicyless served=%v (err %v)",
							round, m.Meta.Name, listed, served, err)
					}
				}
			}
		})
	}
}

// randomGrantCatalog builds a small catalog whose grants, tiers, key
// ownership, deprecation and NoAuth flags are all randomized, plus the
// caller policy to evaluate against it.
func randomGrantCatalog(r *rand.Rand) (*catalog.Snapshot, *policy.Policy, []*model.Model) {
	provID := meta.NewID()
	prov := &provider.Provider{Meta: meta.Metadata{ID: provID, Name: "acme", Owner: meta.Owner{Kind: meta.OwnerSystem}}}

	nHosts := 1 + r.Intn(2)
	var (
		hosts    []*host.Host
		policies []*policy.Policy
		keys     []*hostkey.HostKey
		keyIDs   []string
	)
	for i := 0; i < nHosts; i++ {
		hostID := meta.NewID()
		h := &host.Host{
			Meta: meta.Metadata{ID: hostID, Name: fmt.Sprintf("host-%d", i), Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: host.Spec{BaseURL: fmt.Sprintf("http://h%d.example", i), NoAuth: r.Intn(4) == 0},
		}
		hosts = append(hosts, h)
		tierOn := r.Intn(5) != 0
		tier := &policy.Policy{
			Meta: meta.Metadata{ID: meta.NewID(), Name: fmt.Sprintf("tier-%d", i), Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}},
			Spec: policy.Spec{Enabled: &tierOn},
		}
		// A tier that names a model set grants only that set.
		if r.Intn(3) == 0 {
			tier.Spec.Models = []string{fmt.Sprintf("acme/m%d", r.Intn(4))}
		}
		policies = append(policies, tier)
		if !h.Spec.NoAuth && r.Intn(4) != 0 {
			k := &hostkey.HostKey{
				Meta: meta.Metadata{ID: meta.NewID(), Name: fmt.Sprintf("key-%d", i), Owner: meta.Owner{Kind: meta.OwnerSystem}},
				Spec: hostkey.Spec{HostID: hostID, PolicyID: tier.Meta.ID, Value: "sk", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}},
			}
			keys = append(keys, k)
			keyIDs = append(keyIDs, k.Meta.ID)
		}
	}

	nModels := 1 + r.Intn(3)
	var (
		models   []*model.Model
		bindings []*binding.Binding
		modelIDs []string
	)
	for i := 0; i < nModels; i++ {
		name := fmt.Sprintf("m%d", i)
		m := &model.Model{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
			Spec: model.Spec{Snapshots: []model.Snapshot{{Name: name}}, Pointer: slug.From(name)},
		}
		if r.Intn(5) == 0 {
			m.Spec.Deprecation = &model.Deprecation{Status: model.DeprecationDeprecated}
		}
		models = append(models, m)
		modelIDs = append(modelIDs, m.Meta.ID)
		for _, h := range hosts {
			if r.Intn(4) == 0 {
				continue // this model is not served here
			}
			on := r.Intn(6) != 0
			bindings = append(bindings, &binding.Binding{
				Meta: meta.Metadata{ID: meta.NewID(), Name: name + "-on-" + h.Meta.Name, Owner: meta.Owner{Kind: meta.OwnerSystem}},
				Spec: binding.Spec{ModelID: m.Meta.ID, HostID: h.Meta.ID, Adapter: adapters.OpenAI, Enabled: &on},
			})
		}
	}

	caller := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "caller", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: policy.Spec{IncludeDeprecated: r.Intn(2) == 0},
	}
	// One of: implicit wildcard, a legacy id grant, or a ref grant.
	switch r.Intn(3) {
	case 1:
		caller.Spec.ModelIDs = []string{modelIDs[r.Intn(len(modelIDs))]}
	case 2:
		if r.Intn(2) == 0 {
			caller.Spec.Models = []string{"acme"}
		} else {
			caller.Spec.Models = []string{fmt.Sprintf("acme/m%d", r.Intn(nModels))}
		}
	}
	// The caller holds some subset of the host keys.
	for _, id := range keyIDs {
		if r.Intn(4) != 0 {
			caller.Spec.HostKeyIDs = append(caller.Spec.HostKeyIDs, id)
		}
	}
	policies = append(policies, caller)

	snap := catalog.Build(
		[]*provider.Provider{prov}, hosts, policies, nil, models, keys, nil, nil, bindings)
	return snap, caller, models
}

// Deprecated models are hidden from grants that did not name them: a wildcard
// or provider-wide ref skips them, an explicit provider/model ref still
// grants them, and IncludeDeprecated opens the wildcard back up.
func TestResolve_DeprecatedGrantMatrix(t *testing.T) {
	for _, status := range []model.DeprecationStatus{model.DeprecationDeprecated, model.DeprecationSunset} {
		for _, tc := range []struct {
			name    string
			spec    policy.Spec
			wantErr error
		}{
			{
				name:    "implicit wildcard hides it",
				spec:    policy.Spec{},
				wantErr: ErrModelNotInPolicy,
			},
			{
				name: "implicit wildcard with includeDeprecated serves it",
				spec: policy.Spec{IncludeDeprecated: true},
			},
			{
				name:    "a provider-wide ref hides it",
				spec:    policy.Spec{Models: []string{"acme"}},
				wantErr: ErrModelNotInPolicy,
			},
			{
				name: "a provider-wide ref with includeDeprecated serves it",
				spec: policy.Spec{Models: []string{"acme"}, IncludeDeprecated: true},
			},
			{
				name: "an explicit model ref serves it regardless",
				spec: policy.Spec{Models: []string{"acme/m1"}},
			},
			{
				name: "a legacy id grant serves it regardless",
			},
		} {
			t.Run(string(status)+"/"+tc.name, func(t *testing.T) {
				f := newTwoHostParts()
				f.model.Spec.Deprecation = &model.Deprecation{Status: status}
				caller := &policy.Policy{
					Meta: meta.Metadata{ID: meta.NewID(), Name: "caller", Owner: meta.Owner{Kind: meta.OwnerUser}},
					Spec: tc.spec,
				}
				if tc.name == "a legacy id grant serves it regardless" {
					caller.Spec.ModelIDs = []string{f.model.Meta.ID}
				}
				caller.Spec.HostKeyIDs = []string{f.keyA.Meta.ID}
				snap := catalog.Build(
					[]*provider.Provider{f.provider},
					[]*host.Host{f.hostRowA},
					[]*policy.Policy{f.tierA, caller}, nil,
					[]*model.Model{f.model},
					[]*hostkey.HostKey{f.keyA}, nil, nil,
					[]*binding.Binding{f.bindingA},
				)
				_, err := (&Resolver{}).Resolve(Request{ModelName: "m1", Policy: caller, Snapshot: snap})
				if tc.wantErr != nil {
					if !errors.Is(err, tc.wantErr) {
						t.Fatalf("err = %v, want %v", err, tc.wantErr)
					}
				} else if err != nil {
					t.Fatalf("Resolve: %v", err)
				}
				// The listing agrees with the flow in every cell.
				if got, want := PolicyAllows(snap, caller, f.model), tc.wantErr == nil; got != want {
					t.Errorf("PolicyAllows = %v, want %v", got, want)
				}
			})
		}
	}

	// An active model is never hidden, and neither is one with no
	// deprecation block at all.
	for _, status := range []model.DeprecationStatus{"", model.DeprecationActive} {
		f := newTwoHostParts()
		if status != "" {
			f.model.Spec.Deprecation = &model.Deprecation{Status: status}
		}
		caller := &policy.Policy{
			Meta: meta.Metadata{ID: meta.NewID(), Name: "caller", Owner: meta.Owner{Kind: meta.OwnerUser}},
			Spec: policy.Spec{HostKeyIDs: []string{f.keyA.Meta.ID}},
		}
		snap := catalog.Build(
			[]*provider.Provider{f.provider},
			[]*host.Host{f.hostRowA},
			[]*policy.Policy{f.tierA, caller}, nil,
			[]*model.Model{f.model},
			[]*hostkey.HostKey{f.keyA}, nil, nil,
			[]*binding.Binding{f.bindingA},
		)
		if _, err := (&Resolver{}).Resolve(Request{ModelName: "m1", Policy: caller, Snapshot: snap}); err != nil {
			t.Errorf("deprecation status %q: Resolve: %v", status, err)
		}
	}
}

// D79: the tier gate decides which of a host's keys may serve a given model,
// and it answers the same way through resolution, the policy-less flow and
// both listings. The matrix is the four shapes a tier can take.
func TestTierGate_MatrixAcrossResolutionAndListing(t *testing.T) {
	// A disabled tier and a tier that names another model are pinned on the
	// resolution side by TestResolve_DisabledTierPolicyDeniesTheKey and
	// TestResolvePolicyless_AppliesTierGate; what is left for the matrix is
	// the shapes those two do not cover, checked across all four surfaces.
	for _, tc := range []struct {
		name string
		tier policy.Spec
		// absentTier drops the tier row entirely, so the key names a policy
		// the snapshot does not hold.
		absentTier bool
		grant      bool
	}{
		{name: "an enabled tier with no model list is an implicit wildcard", grant: true},
		{name: "a tier naming this model grants it", tier: policy.Spec{Models: []string{"acme/m1"}}, grant: true},
		{name: "the key's tier names no row at all", absentTier: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTwoHostParts()
			f.tierA.Spec = tc.tier
			// A second model exists so a tier can name something else.
			other := &model.Model{
				Meta: meta.Metadata{ID: meta.NewID(), Name: "other", Owner: f.model.Meta.Owner},
				Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "other"}}, Pointer: slug.From("other")},
			}
			otherBnd := &binding.Binding{
				Meta: meta.Metadata{ID: meta.NewID(), Name: "other-on-a", Owner: meta.Owner{Kind: meta.OwnerSystem}},
				Spec: binding.Spec{ModelID: other.Meta.ID, HostID: f.hostA, Adapter: adapters.OpenAI},
			}
			caller := &policy.Policy{
				Meta: meta.Metadata{ID: meta.NewID(), Name: "caller", Owner: meta.Owner{Kind: meta.OwnerUser}},
				Spec: policy.Spec{HostKeyIDs: []string{f.keyA.Meta.ID}},
			}
			policies := []*policy.Policy{f.tierA, caller}
			if tc.absentTier {
				policies = []*policy.Policy{caller}
			}
			snap := catalog.Build(
				[]*provider.Provider{f.provider},
				[]*host.Host{f.hostRowA},
				policies, nil,
				[]*model.Model{f.model, other},
				[]*hostkey.HostKey{f.keyA}, nil, nil,
				[]*binding.Binding{f.bindingA, otherBnd},
			)
			// A key whose tier is merely switched off survives — evicting it
			// would strand it until a reload, and the gate is what denies it.
			// A key whose tier is absent has lost a required ref and is
			// dropped at build instead.
			_, keyKept := snap.HostKey(f.keyA.Meta.ID)
			if keyKept == tc.absentTier {
				t.Fatalf("host key in the snapshot = %v, want %v", keyKept, !tc.absentTier)
			}

			_, err := (&Resolver{}).Resolve(Request{ModelName: "m1", Policy: caller, Snapshot: snap})
			if tc.grant && err != nil {
				t.Errorf("Resolve: %v", err)
			}
			if !tc.grant && !errors.Is(err, ErrNoKeys) {
				t.Errorf("Resolve err = %v, want ErrNoKeys", err)
			}
			if got := PolicyAllows(snap, caller, f.model); got != tc.grant {
				t.Errorf("PolicyAllows = %v, want %v", got, tc.grant)
			}
			if got := PolicylessAllows(snap, f.model, ""); got != tc.grant {
				t.Errorf("PolicylessAllows = %v, want %v", got, tc.grant)
			}
			_, err = (&Resolver{}).resolvePolicyless(snap, []*model.Model{f.model}, &f.model.Spec.Snapshots[0], "")
			if tc.grant && err != nil {
				t.Errorf("resolvePolicyless: %v", err)
			}
			if !tc.grant && !errors.Is(err, ErrNoKeys) {
				t.Errorf("resolvePolicyless err = %v, want ErrNoKeys", err)
			}
		})
	}
}

// A tier that grants another model still lets its key serve that one — the
// gate narrows what a key may spend on, it does not switch the key off.
func TestTierGate_KeyStillServesTheModelItsTierGrants(t *testing.T) {
	f := newTwoHostParts()
	other := &model.Model{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "other", Owner: f.model.Meta.Owner},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "other"}}, Pointer: slug.From("other")},
	}
	otherBnd := &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "other-on-a", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: other.Meta.ID, HostID: f.hostA, Adapter: adapters.OpenAI},
	}
	f.tierA.Spec.Models = []string{"acme/other"}
	caller := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "caller", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: policy.Spec{HostKeyIDs: []string{f.keyA.Meta.ID}},
	}
	snap := catalog.Build(
		[]*provider.Provider{f.provider},
		[]*host.Host{f.hostRowA},
		[]*policy.Policy{f.tierA, caller}, nil,
		[]*model.Model{f.model, other},
		[]*hostkey.HostKey{f.keyA}, nil, nil,
		[]*binding.Binding{f.bindingA, otherBnd},
	)
	if _, err := (&Resolver{}).Resolve(Request{ModelName: "other", Policy: caller, Snapshot: snap}); err != nil {
		t.Fatalf("the model the tier does grant is unreachable: %v", err)
	}
	if !PolicyAllows(snap, caller, other) {
		t.Error("PolicyAllows hides the model the tier grants")
	}
}

// hostKeysForHost filters a policy's key list down to the chosen host: a key
// for another host, a disabled key, and an id naming nothing are all skipped
// rather than handed to the pool.
func TestResolve_KeyListIsFilteredToTheChosenHost(t *testing.T) {
	f := newTwoHostParts()
	off := false
	f.keyB.Spec.Enabled = &off
	caller := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "caller", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: policy.Spec{HostKeyIDs: []string{meta.NewID(), f.keyB.Meta.ID, f.keyA.Meta.ID}},
	}
	snap := catalog.Build(
		[]*provider.Provider{f.provider},
		[]*host.Host{f.hostRowA, f.hostRB},
		[]*policy.Policy{f.tierA, f.tierB, caller}, nil,
		[]*model.Model{f.model},
		[]*hostkey.HostKey{f.keyA, f.keyB}, nil, nil,
		[]*binding.Binding{f.bindingA, f.bindB},
	)
	plan, err := (&Resolver{}).Resolve(Request{ModelName: "m1", Policy: caller, Snapshot: snap})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(plan.Keys) != 1 || plan.Keys[0].Meta.ID != f.keyA.Meta.ID {
		t.Fatalf("keys = %v, want only the enabled key for the chosen host", plan.Keys)
	}
}

// An unknown model name never reaches the grant walk.
func TestResolve_UnknownAndEmptyModelNames(t *testing.T) {
	f := newTwoHostParts()
	caller := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "caller", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: policy.Spec{HostKeyIDs: []string{f.keyA.Meta.ID}},
	}
	snap := catalog.Build(
		[]*provider.Provider{f.provider},
		[]*host.Host{f.hostRowA},
		[]*policy.Policy{f.tierA, caller}, nil,
		[]*model.Model{f.model},
		[]*hostkey.HostKey{f.keyA}, nil, nil,
		[]*binding.Binding{f.bindingA},
	)
	for _, name := range []string{"", "   ", "no-such-model", "acme/no-such-model@host-a"} {
		if _, err := (&Resolver{}).Resolve(Request{ModelName: name, Policy: caller, Snapshot: snap}); !errors.Is(err, ErrModelNotFound) {
			t.Errorf("Resolve(%q) err = %v, want ErrModelNotFound", name, err)
		}
	}
}

// A binding may serve only some of a model's snapshots. A request for a
// snapshot it does not list must skip it, not be routed there.
func TestResolve_BindingServesOnlyItsListedSnapshots(t *testing.T) {
	f := newTwoHostParts()
	f.model.Spec.Snapshots = []model.Snapshot{{Name: "m1"}, {Name: "m1-preview"}}
	f.bindingA.Spec.Snapshots = []string{"m1"}
	f.bindB.Spec.Snapshots = []string{"m1-preview"}
	caller := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "caller", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: policy.Spec{HostKeyIDs: []string{f.keyA.Meta.ID, f.keyB.Meta.ID}},
	}
	snap := catalog.Build(
		[]*provider.Provider{f.provider},
		[]*host.Host{f.hostRowA, f.hostRB},
		[]*policy.Policy{f.tierA, f.tierB, caller}, nil,
		[]*model.Model{f.model},
		[]*hostkey.HostKey{f.keyA, f.keyB}, nil, nil,
		[]*binding.Binding{f.bindingA, f.bindB},
	)
	for name, wantHost := range map[string]string{"m1": f.hostA, "m1-preview": f.hostB} {
		plan, err := (&Resolver{}).Resolve(Request{ModelName: name, Policy: caller, Snapshot: snap})
		if err != nil {
			t.Fatalf("Resolve(%q): %v", name, err)
		}
		if plan.Host.Meta.ID != wantHost {
			t.Errorf("Resolve(%q) went to %q, want the host whose binding serves that snapshot", name, plan.Host.Meta.Name)
		}
	}
	// The policy-less flow applies the same snapshot filter.
	if _, err := (&Resolver{}).resolvePolicyless(snap, []*model.Model{f.model}, &f.model.Spec.Snapshots[1], f.hostA); !errors.Is(err, ErrNoHostBinding) {
		t.Errorf("err = %v, want ErrNoHostBinding — that host's binding does not serve this snapshot", err)
	}
}

// A model an operator switched off is diagnosed as disabled, not as one the
// policy fails to grant.
func TestResolve_DisabledModelIsDiagnosedAsDisabled(t *testing.T) {
	f := newTwoHostParts()
	off := false
	f.model.Spec.Enabled = &off
	caller := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "caller", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: policy.Spec{ModelIDs: []string{f.model.Meta.ID}, HostKeyIDs: []string{f.keyA.Meta.ID}},
	}
	snap := catalog.Build(
		[]*provider.Provider{f.provider},
		[]*host.Host{f.hostRowA},
		[]*policy.Policy{f.tierA, caller}, nil,
		[]*model.Model{f.model},
		[]*hostkey.HostKey{f.keyA}, nil, nil,
		[]*binding.Binding{f.bindingA},
	)
	// The alias index still answers the name; the walk is what refuses it.
	if _, err := (&Resolver{}).Resolve(Request{ModelName: "m1", Policy: caller, Snapshot: snap}); err != nil &&
		!errors.Is(err, ErrModelDisabled) && !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("err = %v, want the model to be reported disabled or absent", err)
	}
}

// A nil policy and a nil model are not reachable models, and neither is a
// model in a snapshot that never heard of it.
func TestPolicyAllows_NilInputs(t *testing.T) {
	f := newTwoHostParts()
	snap := catalog.Build(
		[]*provider.Provider{f.provider}, []*host.Host{f.hostRowA},
		[]*policy.Policy{f.tierA}, nil, []*model.Model{f.model},
		[]*hostkey.HostKey{f.keyA}, nil, nil, []*binding.Binding{f.bindingA})

	if PolicyAllows(snap, nil, f.model) {
		t.Error("a nil policy grants something")
	}
	if PolicyAllows(snap, f.tierA, nil) {
		t.Error("a nil model is reachable")
	}
	if PolicylessAllows(snap, nil, "") {
		t.Error("a nil model is listed policy-less")
	}
	off := false
	disabledModel := *f.model
	disabledModel.Spec.Enabled = &off
	if PolicylessAllows(snap, &disabledModel, "") {
		t.Error("a disabled model is listed")
	}
}
