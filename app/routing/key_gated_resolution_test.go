package routing

import (
	"context"
	"errors"
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

// twoHostFixture builds one model served by two hosts, both keyed by their
// own tier policy, and returns the ids the tests pin behaviour on.
type twoHostFixture struct {
	model            *model.Model
	hostA, hostB     string
	keyA, keyB       *hostkey.HostKey
	tierA, tierB     *policy.Policy
	provider         *provider.Provider
	bindingA, bindB  *binding.Binding
	hostRowA, hostRB *host.Host
}

func newTwoHostParts() twoHostFixture {
	provID, hostA, hostB, modID := meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID()
	tierAID, tierBID := meta.NewID(), meta.NewID()
	f := twoHostFixture{hostA: hostA, hostB: hostB}
	f.provider = &provider.Provider{Meta: meta.Metadata{ID: provID, Name: "acme", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	f.hostRowA = &host.Host{
		Meta: meta.Metadata{ID: hostA, Name: "host-a", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "http://a.example"},
	}
	f.hostRB = &host.Host{
		Meta: meta.Metadata{ID: hostB, Name: "host-b", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "http://b.example"},
	}
	f.model = &model.Model{
		Meta: meta.Metadata{ID: modID, Name: "m1", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "m1"}}, Pointer: slug.From("m1")},
	}
	f.bindingA = &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "m1-on-a", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: modID, HostID: hostA, Adapter: adapters.OpenAI},
	}
	f.bindB = &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "m1-on-b", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: modID, HostID: hostB, Adapter: adapters.OpenAI},
	}
	f.tierA = &policy.Policy{Meta: meta.Metadata{ID: tierAID, Name: "tier-a", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostA}}}
	f.tierB = &policy.Policy{Meta: meta.Metadata{ID: tierBID, Name: "tier-b", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostB}}}
	f.keyA = &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "key-a", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: hostkey.Spec{HostID: hostA, PolicyID: tierAID, Value: "sk-a", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}},
	}
	f.keyB = &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "key-b", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: hostkey.Spec{HostID: hostB, PolicyID: tierBID, Value: "sk-b", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}},
	}
	return f
}

// The first binding the policy allows has no key the policy may spend;
// resolution must keep walking to the binding it does hold a key for instead
// of answering ErrNoKeys.
func TestResolve_WalksPastKeylessBinding(t *testing.T) {
	f := newTwoHostParts()
	caller := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "caller", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: policy.Spec{HostKeyIDs: []string{f.keyB.Meta.ID}},
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
	if plan.Host.Meta.ID != f.hostB {
		t.Fatalf("chose host %q, want the one the policy holds a key for (%q)", plan.Host.Meta.Name, "host-b")
	}
	if len(plan.Keys) != 1 || plan.Keys[0].Meta.Name != "key-b" {
		t.Fatalf("keys = %v, want [key-b]", plan.Keys)
	}
}

// A policy holding no key for any allowed binding still answers
// ErrNoKeys — the walk must not turn a keyless model into "not in policy".
func TestResolve_NoKeysAnywhereStillAnswersNoKeys(t *testing.T) {
	f := newTwoHostParts()
	caller := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "caller", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: policy.Spec{Models: []string{"acme/m1"}},
	}
	snap := catalog.Build(
		[]*provider.Provider{f.provider},
		[]*host.Host{f.hostRowA, f.hostRB},
		[]*policy.Policy{f.tierA, f.tierB, caller}, nil,
		[]*model.Model{f.model},
		[]*hostkey.HostKey{f.keyA, f.keyB}, nil, nil,
		[]*binding.Binding{f.bindingA, f.bindB},
	)
	_, err := (&Resolver{}).Resolve(Request{ModelName: "m1", Policy: caller, Snapshot: snap})
	if !errors.Is(err, ErrNoKeys) {
		t.Fatalf("err = %v, want ErrNoKeys", err)
	}
}

// lister serves a fixed slice to catalog.New / UseTenancy, so a test can
// build a snapshot that actually holds tenancy rows.
type lister[T any] []*T

func (l lister[T]) List(context.Context) ([]*T, error) { return l, nil }

// D73: a policy-less request draws only on system- or user-owned host keys.
// A project's credential is reachable only through that project's policy,
// which is what holds the spend inside its limits and attribution — so the
// key here is live and its project present, and it is still not a candidate.
func TestResolvePolicyless_SkipsProjectOwnedKeys(t *testing.T) {
	f := newTwoHostParts()
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	proj := &project.Project{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "ml"},
		Spec: project.Spec{TeamID: tm.Meta.ID},
	}
	proj.StampOwner()
	f.keyA.Meta.Owner = meta.Owner{Kind: meta.OwnerProject, ID: proj.Meta.ID}

	c := catalog.New(
		lister[provider.Provider]{f.provider}, lister[host.Host]{f.hostRowA},
		lister[policy.Policy]{f.tierA}, lister[model.Model]{f.model},
		lister[hostkey.HostKey]{f.keyA}, lister[ratelimit.RateLimit]{},
		lister[key.Key]{}, lister[pricing.Pricing]{}, lister[binding.Binding]{f.bindingA},
	)
	c.UseTenancy(lister[team.Team]{tm}, lister[project.Project]{proj},
		lister[serviceaccount.ServiceAccount]{}, lister[group.Group]{},
		lister[role.Role]{}, lister[rolebinding.RoleBinding]{}, lister[policybinding.PolicyBinding]{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	snap := c.Current()
	if _, ok := snap.HostKey(f.keyA.Meta.ID); !ok {
		t.Fatal("the project-owned key was dropped before the pool filter could be tested")
	}

	_, err := (&Resolver{}).resolvePolicyless(snap, []*model.Model{f.model}, &f.model.Spec.Snapshots[0], "")
	if !errors.Is(err, ErrNoKeys) {
		t.Fatalf("err = %v, want ErrNoKeys — a project key must not serve policy-less traffic", err)
	}
}

// D73: the tier gate applies to the policy-less pool too: a key whose
// host tier does not grant the model is not a usable candidate.
func TestResolvePolicyless_AppliesTierGate(t *testing.T) {
	f := newTwoHostParts()
	// A tier that grants a different model only.
	f.tierA.Spec.Models = []string{"acme/other"}
	other := &model.Model{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "other", Owner: f.model.Meta.Owner},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "other"}}, Pointer: slug.From("other")},
	}
	otherBnd := &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "other-on-a", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: other.Meta.ID, HostID: f.hostA, Adapter: adapters.OpenAI},
	}
	snap := catalog.Build(
		[]*provider.Provider{f.provider},
		[]*host.Host{f.hostRowA},
		[]*policy.Policy{f.tierA}, nil,
		[]*model.Model{f.model, other},
		[]*hostkey.HostKey{f.keyA}, nil, nil,
		[]*binding.Binding{f.bindingA, otherBnd},
	)
	_, err := (&Resolver{}).resolvePolicyless(snap, []*model.Model{f.model}, &f.model.Spec.Snapshots[0], "")
	if !errors.Is(err, ErrNoKeys) {
		t.Fatalf("err = %v, want ErrNoKeys — the tier does not grant this model", err)
	}
}

// The listing gate is Resolve's gate: a model the policy names but
// holds no usable key for must not be advertised.
func TestPolicyAllows_RequiresAKeyResolveWouldUse(t *testing.T) {
	f := newTwoHostParts()
	caller := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "caller", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: policy.Spec{Models: []string{"acme/m1"}},
	}
	snap := catalog.Build(
		[]*provider.Provider{f.provider},
		[]*host.Host{f.hostRowA},
		[]*policy.Policy{f.tierA, caller}, nil,
		[]*model.Model{f.model},
		[]*hostkey.HostKey{f.keyA}, nil, nil,
		[]*binding.Binding{f.bindingA},
	)
	if PolicyAllows(snap, caller, f.model) {
		t.Fatal("PolicyAllows = true, but the policy holds no host key for the model's host")
	}
	caller.Spec.HostKeyIDs = []string{f.keyA.Meta.ID}
	snap = catalog.Build(
		[]*provider.Provider{f.provider},
		[]*host.Host{f.hostRowA},
		[]*policy.Policy{f.tierA, caller}, nil,
		[]*model.Model{f.model},
		[]*hostkey.HostKey{f.keyA}, nil, nil,
		[]*binding.Binding{f.bindingA},
	)
	if !PolicyAllows(snap, caller, f.model) {
		t.Fatal("PolicyAllows = false with a granted, keyed model")
	}

	// Host-key coverage alone is not the gate Resolve applies: the key's own
	// tier has to grant this model too, or the listing advertises a model
	// every request for it would answer ErrNoKeys.
	f.tierA.Spec.Models = []string{"acme/other"}
	other := &model.Model{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "other", Owner: f.model.Meta.Owner},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "other"}}, Pointer: slug.From("other")},
	}
	otherBnd := &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "other-on-a", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: other.Meta.ID, HostID: f.hostA, Adapter: adapters.OpenAI},
	}
	snap = catalog.Build(
		[]*provider.Provider{f.provider},
		[]*host.Host{f.hostRowA},
		[]*policy.Policy{f.tierA, caller}, nil,
		[]*model.Model{f.model, other},
		[]*hostkey.HostKey{f.keyA}, nil, nil,
		[]*binding.Binding{f.bindingA, otherBnd},
	)
	if PolicyAllows(snap, caller, f.model) {
		t.Fatal("PolicyAllows = true, but the key's tier policy does not grant this model")
	}
	if _, err := (&Resolver{}).Resolve(Request{ModelName: "m1", Policy: caller, Snapshot: snap}); !errors.Is(err, ErrNoKeys) {
		t.Fatalf("Resolve err = %v, want ErrNoKeys — the listing and Resolve must agree", err)
	}
}

// An explicit grant on a NoAuth host is reachable — Resolve serves it
// with the synthetic anonymous key — so the listing must show it.
func TestPolicyAllows_ListsNoAuthHost(t *testing.T) {
	f := newTwoHostParts()
	f.hostRowA.Spec.NoAuth = true
	caller := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "caller", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: policy.Spec{Models: []string{"acme/m1"}},
	}
	snap := catalog.Build(
		[]*provider.Provider{f.provider},
		[]*host.Host{f.hostRowA},
		[]*policy.Policy{caller}, nil,
		[]*model.Model{f.model},
		nil, nil, nil,
		[]*binding.Binding{f.bindingA},
	)
	if !PolicyAllows(snap, caller, f.model) {
		t.Fatal("PolicyAllows = false for an explicitly granted model on a NoAuth host")
	}
}

// A disabled policy grants nothing, listing included.
func TestPolicyAllows_DisabledPolicyGrantsNothing(t *testing.T) {
	f := newTwoHostParts()
	off := false
	caller := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "caller", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: policy.Spec{HostKeyIDs: []string{f.keyA.Meta.ID}, Enabled: &off},
	}
	snap := catalog.Build(
		[]*provider.Provider{f.provider},
		[]*host.Host{f.hostRowA},
		[]*policy.Policy{f.tierA, caller}, nil,
		[]*model.Model{f.model},
		[]*hostkey.HostKey{f.keyA}, nil, nil,
		[]*binding.Binding{f.bindingA},
	)
	if PolicyAllows(snap, caller, f.model) {
		t.Fatal("PolicyAllows = true for a disabled policy")
	}
}

// D79: a key whose tier policy is switched off is not a candidate. The key
// itself stays in the snapshot — evicting it would strand it until a reload —
// and the tier gate is what denies it, so Resolve answers ErrNoKeys rather
// than spending a key it has no rules to meter by.
func TestResolve_DisabledTierPolicyDeniesTheKey(t *testing.T) {
	f := newTwoHostParts()
	off := false
	f.tierA.Spec.Enabled = &off
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
	if _, ok := snap.HostKey(f.keyA.Meta.ID); !ok {
		t.Fatal("the key was evicted; the tier gate should be what denies it")
	}
	_, err := (&Resolver{}).Resolve(Request{ModelName: "m1", Policy: caller, Snapshot: snap})
	if !errors.Is(err, ErrNoKeys) {
		t.Fatalf("err = %v, want ErrNoKeys", err)
	}
}

// D79: re-enabling the tier restores service with no reload — the key never
// left, so the gate simply stops denying it.
func TestResolve_ReEnabledTierServesAgain(t *testing.T) {
	f := newTwoHostParts()
	off, on := false, true
	f.tierA.Spec.Enabled = &off
	caller := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "caller", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: policy.Spec{HostKeyIDs: []string{f.keyA.Meta.ID}},
	}
	c := catalog.New(
		lister[provider.Provider]{f.provider}, lister[host.Host]{f.hostRowA},
		lister[policy.Policy]{f.tierA, caller}, lister[model.Model]{f.model},
		lister[hostkey.HostKey]{f.keyA}, lister[ratelimit.RateLimit]{},
		lister[key.Key]{}, lister[pricing.Pricing]{}, lister[binding.Binding]{f.bindingA},
	)
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	r := New(c)
	if _, err := r.Resolve(Request{ModelName: "m1", Policy: caller}); !errors.Is(err, ErrNoKeys) {
		t.Fatalf("err = %v, want ErrNoKeys while the tier is off", err)
	}

	turnedOn := *f.tierA
	turnedOn.Spec.Enabled = &on
	if err := c.ApplyPolicyUpsert(&turnedOn); err != nil {
		t.Fatalf("re-enable tier policy: %v", err)
	}
	plan, err := r.Resolve(Request{ModelName: "m1", Policy: caller})
	if err != nil {
		t.Fatalf("Resolve after re-enabling the tier: %v — the key is stranded", err)
	}
	if len(plan.Keys) != 1 || plan.Keys[0].Meta.ID != f.keyA.Meta.ID {
		t.Fatalf("keys = %v, want the key back in service", plan.Keys)
	}
}

// D77: Resolve is handed a disabled policy — the middleware resolves
// it rather than falling through — and answers ErrPolicyDisabled.
func TestResolve_DisabledPolicyIsReachable(t *testing.T) {
	f := newTwoHostParts()
	off := false
	caller := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "caller", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: policy.Spec{HostKeyIDs: []string{f.keyA.Meta.ID}, Enabled: &off},
	}
	snap := catalog.Build(
		[]*provider.Provider{f.provider},
		[]*host.Host{f.hostRowA},
		[]*policy.Policy{f.tierA, caller}, nil,
		[]*model.Model{f.model},
		[]*hostkey.HostKey{f.keyA}, nil, nil,
		[]*binding.Binding{f.bindingA},
	)
	_, err := (&Resolver{}).Resolve(Request{ModelName: "m1", Policy: caller, Snapshot: snap})
	if !errors.Is(err, ErrPolicyDisabled) {
		t.Fatalf("err = %v, want ErrPolicyDisabled", err)
	}
}

// D73: the policy-less listing answers exactly what the policy-less flow
// would serve. A project-owned key is not in that pool, so its model is not
// advertised; a NoAuth host is served by the anonymous key, so its model is.
func TestPolicylessAllows_MatchesTheFlowThatServesIt(t *testing.T) {
	f := newTwoHostParts()
	shared := catalog.Build(
		[]*provider.Provider{f.provider},
		[]*host.Host{f.hostRowA},
		[]*policy.Policy{f.tierA}, nil,
		[]*model.Model{f.model},
		[]*hostkey.HostKey{f.keyA}, nil, nil,
		[]*binding.Binding{f.bindingA},
	)
	if !PolicylessAllows(shared, f.model, "") {
		t.Fatal("a system-owned key's model is not listed")
	}

	// A NoAuth host has no host key at all and is still reachable.
	noAuth := newTwoHostParts()
	noAuth.hostRowA.Spec.NoAuth = true
	open := catalog.Build(
		[]*provider.Provider{noAuth.provider},
		[]*host.Host{noAuth.hostRowA},
		nil, nil,
		[]*model.Model{noAuth.model},
		nil, nil, nil,
		[]*binding.Binding{noAuth.bindingA},
	)
	if !PolicylessAllows(open, noAuth.model, "") {
		t.Error("a NoAuth host's model is not listed, but the flow serves it with the anonymous key")
	}
	if _, err := (&Resolver{}).resolvePolicyless(open, []*model.Model{noAuth.model}, &noAuth.model.Spec.Snapshots[0], ""); err != nil {
		t.Errorf("resolvePolicyless on the NoAuth host: %v", err)
	}

	// A tier that grants a different model takes the key out of the pool, so
	// the model stops being listed too.
	gated := newTwoHostParts()
	gated.tierA.Spec.Models = []string{"acme/other"}
	other := &model.Model{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "other", Owner: gated.model.Meta.Owner},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "other"}}, Pointer: slug.From("other")},
	}
	otherBnd := &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "other-on-a", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: other.Meta.ID, HostID: gated.hostA, Adapter: adapters.OpenAI},
	}
	tiered := catalog.Build(
		[]*provider.Provider{gated.provider},
		[]*host.Host{gated.hostRowA},
		[]*policy.Policy{gated.tierA}, nil,
		[]*model.Model{gated.model, other},
		[]*hostkey.HostKey{gated.keyA}, nil, nil,
		[]*binding.Binding{gated.bindingA, otherBnd},
	)
	if PolicylessAllows(tiered, gated.model, "") {
		t.Error("a model the key's tier does not grant is listed anyway")
	}

	// The adapter filter narrows without changing the pool rule.
	if PolicylessAllows(shared, f.model, "not-a-registered-shape") {
		t.Error("the adapter filter is ignored")
	}
}

// BenchmarkResolveTwoBindings covers the candidate walk that key selection
// moved into. The caller holds a key for both hosts, so it resolves at
// the first binding on either side of the change — the common case the
// restructure must not slow down.
func BenchmarkResolveTwoBindings(b *testing.B) {
	f := newTwoHostParts()
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
	r := &Resolver{}
	req := Request{ModelName: "m1", Policy: caller, Snapshot: snap}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.Resolve(req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkResolvePolicyless covers the D73 pool filter + tier gate.
func BenchmarkResolvePolicyless(b *testing.B) {
	f := newTwoHostParts()
	snap := catalog.Build(
		[]*provider.Provider{f.provider},
		[]*host.Host{f.hostRowA},
		[]*policy.Policy{f.tierA}, nil,
		[]*model.Model{f.model},
		[]*hostkey.HostKey{f.keyA}, nil, nil,
		[]*binding.Binding{f.bindingA},
	)
	r := &Resolver{}
	models := []*model.Model{f.model}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.resolvePolicyless(snap, models, &f.model.Spec.Snapshots[0], ""); err != nil {
			b.Fatal(err)
		}
	}
}
