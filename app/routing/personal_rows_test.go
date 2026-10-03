package routing

import (
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/settings"
)

// personalHostFixture is the system model m1 served by system host-a, plus
// user B's own keyless host bound to m1 under a binding that sorts first.
func personalHostFixture(userB string) (twoHostFixture, *host.Host, *catalog.Snapshot) {
	f := newTwoHostParts()
	personal := &host.Host{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "b-box", Owner: meta.Owner{Kind: meta.OwnerUser, ID: userB}},
		Spec: host.Spec{BaseURL: "http://b.attacker.example", NoAuth: true},
	}
	personalBnd := &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "0000", Owner: meta.Owner{Kind: meta.OwnerUser, ID: userB}},
		Spec: binding.Spec{ModelID: f.model.Meta.ID, HostID: personal.Meta.ID, Adapter: adapters.OpenAI},
	}
	snap := catalog.Build(
		[]*provider.Provider{f.provider},
		[]*host.Host{f.hostRowA, personal},
		[]*policy.Policy{f.tierA}, nil,
		[]*model.Model{f.model},
		[]*hostkey.HostKey{f.keyA}, nil, nil,
		[]*binding.Binding{personalBnd, f.bindingA},
	)
	return f, personal, snap
}

type policylessOn struct{}

func (policylessOn) Setting(string) (any, bool) {
	return &settings.Inference{AllowMissingPolicy: true}, true
}

// anyMode is a Resolver with default options, for checks every mode shares.
var anyMode *Resolver

// Under RBAC another user's personal host must never take a share of a grant
// written against the shared catalog: the grant names the model, not whose
// endpoint serves it.
func TestPersonalHostNotRoutableByOtherUsersUnderRBAC(t *testing.T) {
	userA, userB := meta.NewID(), meta.NewID()
	f, personal, snap := personalHostFixture(userB)
	pol := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "a-pol", Owner: meta.Owner{Kind: meta.OwnerUser, ID: userA}},
		Spec: policy.Spec{Models: []string{"acme/m1"}, HostKeyIDs: []string{f.keyA.Meta.ID}},
	}
	snap = catalog.Build(
		[]*provider.Provider{f.provider},
		[]*host.Host{f.hostRowA, personal},
		[]*policy.Policy{f.tierA, pol}, nil,
		[]*model.Model{f.model},
		[]*hostkey.HostKey{f.keyA}, nil, nil,
		snap.AllBindings(),
	)
	r := &Resolver{cfg: policylessOn{}}
	PersonalRowsOwnerOnly()(r)
	for _, caller := range []string{userA, ""} {
		plan, err := r.Resolve(Request{ModelName: "m1", Policy: pol, UserID: caller, Snapshot: snap})
		if err != nil {
			t.Fatalf("caller %q: Resolve: %v", caller, err)
		}
		if plan.Host.Meta.ID == personal.Meta.ID {
			t.Fatalf("caller %q routed to user B's personal host", caller)
		}
		if !r.PolicyAllowsBinding(snap, pol, f.model, snap.AllBindings()[0], caller) {
			continue
		}
		t.Fatalf("caller %q: user B's binding is listed as granted", caller)
	}
	plan, err := r.Resolve(Request{ModelName: "m1", UserID: userA, Snapshot: snap})
	if err != nil {
		t.Fatalf("policy-less Resolve: %v", err)
	}
	if plan.Host.Meta.ID == personal.Meta.ID {
		t.Fatal("policy-less caller A routed to user B's personal host")
	}
	if r.PolicylessAllowsBinding(snap, f.model, snap.AllBindings()[0], userA) {
		t.Fatal("user B's binding is listed to policy-less caller A")
	}
}

// Under RBAC the owner still reaches their own endpoint.
func TestPersonalHostRoutableByItsOwnerUnderRBAC(t *testing.T) {
	userB := meta.NewID()
	f, personal, snap := personalHostFixture(userB)
	r := &Resolver{cfg: policylessOn{}}
	PersonalRowsOwnerOnly()(r)
	plan, err := r.Resolve(Request{ModelName: "m1", UserID: userB, Snapshot: snap})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plan.Host.Meta.ID != personal.Meta.ID {
		t.Fatalf("owner routed to %q, want their own host", plan.Host.Meta.Name)
	}
	if !r.PolicylessAllows(snap, f.model, "", userB) {
		t.Fatal("owner's model not listed")
	}
}

// Under single-user authorization every caller is the operator, so the
// operator's personal host stays reachable by any credential — including a
// service-account key, which carries no user id.
func TestPersonalHostSharedInSingleUserMode(t *testing.T) {
	userB := meta.NewID()
	f, personal, snap := personalHostFixture(userB)
	r := &Resolver{cfg: policylessOn{}}
	for _, caller := range []string{meta.NewID(), ""} {
		plan, err := r.Resolve(Request{ModelName: "m1", UserID: caller, Snapshot: snap})
		if err != nil {
			t.Fatalf("caller %q: Resolve: %v", caller, err)
		}
		if plan.Host.Meta.ID != personal.Meta.ID {
			t.Fatalf("caller %q routed to %q, want the shared personal host (its binding sorts first)", caller, plan.Host.Meta.Name)
		}
		if !r.PolicylessAllowsBinding(snap, f.model, snap.AllBindings()[0], caller) {
			t.Fatalf("caller %q: personal binding not listed", caller)
		}
	}
}
