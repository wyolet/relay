package routing

import (
	"errors"
	"testing"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/provider"
)

// userKeyFixture is newTwoHostParts with hostA's key re-owned by a user, so
// the tests differ only in whose id the caller presents.
func userKeyFixture(t *testing.T, ownerUserID string) (twoHostFixture, *catalog.Snapshot) {
	t.Helper()
	f := newTwoHostParts()
	f.keyA.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: ownerUserID}
	return f, catalog.Build(
		[]*provider.Provider{f.provider},
		[]*host.Host{f.hostRowA},
		[]*policy.Policy{f.tierA}, nil,
		[]*model.Model{f.model},
		[]*hostkey.HostKey{f.keyA}, nil, nil,
		[]*binding.Binding{f.bindingA},
	)
}

// A user's own host key is their credential, not a shared pool: a
// policy-less caller must not spend somebody else's.
func TestResolvePolicyless_RefusesAnotherUsersHostKey(t *testing.T) {
	userB := meta.NewID()
	f, snap := userKeyFixture(t, userB)
	callerA := meta.NewID()

	_, err := (&Resolver{}).resolvePolicyless(snap, []*model.Model{f.model}, &f.model.Spec.Snapshots[0], "", callerA)
	if !errors.Is(err, ErrNoKeys) {
		t.Fatalf("err = %v, want ErrNoKeys — caller %s spent user %s's host key", err, callerA, userB)
	}
	if PolicylessAllows(snap, f.model, "", callerA) {
		t.Error("another user's key makes the model listed to a policy-less caller")
	}
}

// The owner of the key reaches it.
func TestResolvePolicyless_SpendsTheCallersOwnHostKey(t *testing.T) {
	owner := meta.NewID()
	f, snap := userKeyFixture(t, owner)

	plan, err := (&Resolver{}).resolvePolicyless(snap, []*model.Model{f.model}, &f.model.Spec.Snapshots[0], "", owner)
	if err != nil {
		t.Fatalf("resolvePolicyless for the key's own owner: %v", err)
	}
	if len(plan.Keys) != 1 || plan.Keys[0].Meta.ID != f.keyA.Meta.ID {
		t.Fatalf("keys = %v, want the caller's own key", plan.Keys)
	}
	if !PolicylessAllows(snap, f.model, "", owner) {
		t.Error("the caller's own key does not make the model listed")
	}
}

// A system-owned key is the operator's shared pool and stays shared.
func TestResolvePolicyless_SharesSystemOwnedHostKeys(t *testing.T) {
	f := newTwoHostParts() // keyA is system-owned
	snap := catalog.Build(
		[]*provider.Provider{f.provider},
		[]*host.Host{f.hostRowA},
		[]*policy.Policy{f.tierA}, nil,
		[]*model.Model{f.model},
		[]*hostkey.HostKey{f.keyA}, nil, nil,
		[]*binding.Binding{f.bindingA},
	)
	for _, caller := range []string{"", meta.NewID()} {
		plan, err := (&Resolver{}).resolvePolicyless(snap, []*model.Model{f.model}, &f.model.Spec.Snapshots[0], "", caller)
		if err != nil {
			t.Fatalf("caller %q: resolvePolicyless: %v", caller, err)
		}
		if len(plan.Keys) != 1 || plan.Keys[0].Meta.ID != f.keyA.Meta.ID {
			t.Fatalf("caller %q: keys = %v, want the system key", caller, plan.Keys)
		}
	}
}

// No caller id (batch, proxy, any path that never resolved a user) means
// system-owned keys only — never an arbitrary user's credential.
func TestResolvePolicyless_UnknownCallerGetsSystemKeysOnly(t *testing.T) {
	f, snap := userKeyFixture(t, meta.NewID())

	_, err := (&Resolver{}).resolvePolicyless(snap, []*model.Model{f.model}, &f.model.Spec.Snapshots[0], "", "")
	if !errors.Is(err, ErrNoKeys) {
		t.Fatalf("err = %v, want ErrNoKeys — an unknown caller reached a user-owned key", err)
	}
	if PolicylessAllows(snap, f.model, "", "") {
		t.Error("a user-owned key's model is listed to a caller with no id")
	}
}
