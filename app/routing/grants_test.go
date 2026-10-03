package routing_test

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
	"github.com/wyolet/relay/app/routing"
)

// grantsFixture builds one model bound to a single host. noAuth makes the
// host keyless; withKey attaches a hostkey to the policy. grants are the
// policy's modelref grants (empty = implicit wildcard).
func grantsFixture(t *testing.T, noAuth, withKey bool, grants ...string) (*catalog.Snapshot, *policy.Policy, *model.Model) {
	t.Helper()

	provID, modID, hostID := meta.NewID(), meta.NewID(), meta.NewID()
	hkID, polID := meta.NewID(), meta.NewID()

	m := &model.Model{
		Meta: meta.Metadata{ID: modID, Name: "the-model", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "the-model"}}, Pointer: "the-model"},
	}
	h := &host.Host{
		Meta: meta.Metadata{ID: hostID, Name: "the-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "http://x.example", NoAuth: noAuth},
	}
	pol := &policy.Policy{
		Meta: meta.Metadata{ID: polID, Name: "p", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}},
		Spec: policy.Spec{Models: grants},
	}
	var keys []*hostkey.HostKey
	if withKey {
		pol.Spec.HostKeyIDs = []string{hkID}
		keys = append(keys, &hostkey.HostKey{
			Meta: meta.Metadata{ID: hkID, Name: "k", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}},
			Spec: hostkey.Spec{HostID: hostID, PolicyID: polID, Value: "sk-test", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}},
		})
	}
	b := &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "b", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: modID, HostID: hostID, Adapter: adapters.OpenAI},
	}
	snap := catalog.Build(
		[]*provider.Provider{{Meta: meta.Metadata{ID: provID, Name: "prov", Owner: meta.Owner{Kind: meta.OwnerSystem}}}},
		[]*host.Host{h},
		[]*policy.Policy{pol},
		nil,
		[]*model.Model{m},
		keys,
		nil, nil,
		[]*binding.Binding{b},
	)
	stored, ok := snap.Policy(polID)
	if !ok {
		t.Fatal("policy missing from snapshot")
	}
	return snap, stored, m
}

// A keyless upstream needs no HostKey — Resolve injects the anonymous one —
// so an explicit grant is enough to make the model listable.
func TestPolicyAllows_NoAuthHostNeedsNoKey(t *testing.T) {
	snap, pol, m := grantsFixture(t, true, false, "prov/the-model")
	if !routing.PolicyAllows(snap, pol, m) {
		t.Fatal("explicitly granted model on a NoAuth host must be allowed without a hostkey")
	}
}

func TestPolicyAllows_KeyedHostWithoutAKeyIsDenied(t *testing.T) {
	snap, pol, m := grantsFixture(t, false, false, "prov/the-model")
	if routing.PolicyAllows(snap, pol, m) {
		t.Fatal("a host requiring auth must not be listable without a hostkey on the policy")
	}
}

func TestPolicyAllows_KeyedHostWithAKeyIsAllowed(t *testing.T) {
	snap, pol, m := grantsFixture(t, false, true, "prov/the-model")
	if !routing.PolicyAllows(snap, pol, m) {
		t.Fatal("granted model on a keyed host with a key must be allowed")
	}
}

// An implicit wildcard's only real authz is key coverage, which a NoAuth
// host skips — so reaching one takes an explicit grant (mirrors Resolve).
func TestPolicyAllowsBinding_WildcardDoesNotReachNoAuthHost(t *testing.T) {
	snap, pol, m := grantsFixture(t, true, false)
	for _, hb := range snap.BindingsForModel(m.Meta.ID) {
		if routing.PolicyAllowsBinding(snap, pol, m, hb) {
			t.Fatal("an implicit-wildcard policy must not reach an ungranted NoAuth host")
		}
	}
}

func TestPolicyAllowsBinding_NilArgs(t *testing.T) {
	snap, pol, m := grantsFixture(t, true, false, "prov/the-model")
	hb := snap.BindingsForModel(m.Meta.ID)[0]
	if routing.PolicyAllowsBinding(nil, pol, m, hb) ||
		routing.PolicyAllowsBinding(snap, nil, m, hb) ||
		routing.PolicyAllowsBinding(snap, pol, nil, hb) ||
		routing.PolicyAllowsBinding(snap, pol, m, nil) {
		t.Fatal("nil arguments must deny")
	}
}
