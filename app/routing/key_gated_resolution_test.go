package routing

import (
	"errors"
	"testing"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/provider"
)

// The first binding the policy allows has no key the policy may spend;
// resolution must keep walking to the binding it does hold a key for instead
// of answering ErrNoKeys.
func TestResolve_WalksPastKeylessBinding(t *testing.T) {
	f := newTwoHostParts()
	caller := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "caller", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: policy.Spec{HostKeyIDs: []string{f.keyB.Meta.ID}},
	}
	snap := f.twoHostRows(caller).Snapshot()
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
	snap := f.twoHostRows(caller).Snapshot()
	_, err := (&Resolver{}).Resolve(Request{ModelName: "m1", Policy: caller, Snapshot: snap})
	if !errors.Is(err, ErrNoKeys) {
		t.Fatalf("err = %v, want ErrNoKeys", err)
	}
}

// The tier gate applies to the policy-less pool too: a key whose
// host tier does not grant the model is not a usable candidate.
func TestResolvePolicyless_AppliesTierGate(t *testing.T) {
	f := newTwoHostParts()
	// A tier that grants a different model only.
	f.tierA.Spec.Models = []string{"acme/other"}
	other, otherBnd := f.otherModelOnA()
	rows := f.hostARows()
	rows.Models = append(rows.Models, other)
	rows.Bindings = append(rows.Bindings, otherBnd)
	_, err := (&Resolver{}).resolvePolicyless(rows.Snapshot(), []*model.Model{f.model}, &f.model.Spec.Snapshots[0], "", "")
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
	snap := f.hostARows(caller).Snapshot()
	if anyMode.PolicyAllows(snap, caller, f.model) {
		t.Fatal("PolicyAllows = true, but the policy holds no host key for the model's host")
	}
	caller.Spec.HostKeyIDs = []string{f.keyA.Meta.ID}
	snap = f.hostARows(caller).Snapshot()
	if !anyMode.PolicyAllows(snap, caller, f.model) {
		t.Fatal("PolicyAllows = false with a granted, keyed model")
	}

	// Host-key coverage alone is not the gate Resolve applies: the key's own
	// tier has to grant this model too, or the listing advertises a model
	// every request for it would answer ErrNoKeys.
	f.tierA.Spec.Models = []string{"acme/other"}
	other, otherBnd := f.otherModelOnA()
	rows := f.hostARows(caller)
	rows.Models = append(rows.Models, other)
	rows.Bindings = append(rows.Bindings, otherBnd)
	snap = rows.Snapshot()
	if anyMode.PolicyAllows(snap, caller, f.model) {
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
	snap := catalogtest.Catalog{
		Providers: []*provider.Provider{f.provider},
		Hosts:     []*host.Host{f.hostRowA},
		Policies:  []*policy.Policy{caller},
		Models:    []*model.Model{f.model},
		Bindings:  []*binding.Binding{f.bindingA},
	}.Snapshot()
	if !anyMode.PolicyAllows(snap, caller, f.model) {
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
	if anyMode.PolicyAllows(f.hostARows(caller).Snapshot(), caller, f.model) {
		t.Fatal("PolicyAllows = true for a disabled policy")
	}
}

// A key whose tier policy is switched off is not a candidate. The key
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
	snap := f.hostARows(caller).Snapshot()
	if _, ok := snap.HostKey(f.keyA.Meta.ID); !ok {
		t.Fatal("the key was evicted; the tier gate should be what denies it")
	}
	_, err := (&Resolver{}).Resolve(Request{ModelName: "m1", Policy: caller, Snapshot: snap})
	if !errors.Is(err, ErrNoKeys) {
		t.Fatalf("err = %v, want ErrNoKeys", err)
	}
}

// Re-enabling the tier restores service with no reload — the key never
// left, so the gate simply stops denying it.
func TestResolve_ReEnabledTierServesAgain(t *testing.T) {
	f := newTwoHostParts()
	off, on := false, true
	f.tierA.Spec.Enabled = &off
	caller := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "caller", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: policy.Spec{HostKeyIDs: []string{f.keyA.Meta.ID}},
	}
	c := f.hostARows(caller).Load(t)
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

// Resolve is handed a disabled policy — the middleware resolves
// it rather than falling through — and answers ErrPolicyDisabled.
func TestResolve_DisabledPolicyIsReachable(t *testing.T) {
	f := newTwoHostParts()
	off := false
	caller := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "caller", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: policy.Spec{HostKeyIDs: []string{f.keyA.Meta.ID}, Enabled: &off},
	}
	_, err := (&Resolver{}).Resolve(Request{ModelName: "m1", Policy: caller, Snapshot: f.hostARows(caller).Snapshot()})
	if !errors.Is(err, ErrPolicyDisabled) {
		t.Fatalf("err = %v, want ErrPolicyDisabled", err)
	}
}

// The policy-less listing answers exactly what the policy-less flow
// would serve. A project-owned key is not in that pool, so its model is not
// advertised; a NoAuth host is served by the anonymous key, so its model is.
func TestPolicylessAllows_MatchesTheFlowThatServesIt(t *testing.T) {
	f := newTwoHostParts()
	shared := f.hostARows().Snapshot()
	if !anyMode.PolicylessAllows(shared, f.model, "", "") {
		t.Fatal("a system-owned key's model is not listed")
	}

	// A NoAuth host has no host key at all and is still reachable.
	noAuth := newTwoHostParts()
	noAuth.hostRowA.Spec.NoAuth = true
	open := catalogtest.Catalog{
		Providers: []*provider.Provider{noAuth.provider},
		Hosts:     []*host.Host{noAuth.hostRowA},
		Models:    []*model.Model{noAuth.model},
		Bindings:  []*binding.Binding{noAuth.bindingA},
	}.Snapshot()
	if !anyMode.PolicylessAllows(open, noAuth.model, "", "") {
		t.Error("a NoAuth host's model is not listed, but the flow serves it with the anonymous key")
	}
	if _, err := (&Resolver{}).resolvePolicyless(open, []*model.Model{noAuth.model}, &noAuth.model.Spec.Snapshots[0], "", ""); err != nil {
		t.Errorf("resolvePolicyless on the NoAuth host: %v", err)
	}

	// A tier that grants a different model takes the key out of the pool, so
	// the model stops being listed too.
	gated := newTwoHostParts()
	gated.tierA.Spec.Models = []string{"acme/other"}
	other, otherBnd := gated.otherModelOnA()
	rows := gated.hostARows()
	rows.Models = append(rows.Models, other)
	rows.Bindings = append(rows.Bindings, otherBnd)
	if anyMode.PolicylessAllows(rows.Snapshot(), gated.model, "", "") {
		t.Error("a model the key's tier does not grant is listed anyway")
	}

	// The adapter filter narrows without changing the pool rule.
	if anyMode.PolicylessAllows(shared, f.model, "not-a-registered-shape", "") {
		t.Error("the adapter filter is ignored")
	}
	// Which owners' keys are in the pool, for listing and flow alike, is
	// TestResolvePolicyless_KeyPoolScope.
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
	snap := f.twoHostRows(caller).Snapshot()
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

// BenchmarkResolvePolicyless covers the policy-less pool filter + tier gate.
func BenchmarkResolvePolicyless(b *testing.B) {
	f := newTwoHostParts()
	snap := f.hostARows().Snapshot()
	r := &Resolver{}
	models := []*model.Model{f.model}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.resolvePolicyless(snap, models, &f.model.Spec.Snapshots[0], "", ""); err != nil {
			b.Fatal(err)
		}
	}
}
