package catalogview

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
)

// asAlice is a Visible predicate for the tests: alice sees non-user rows
// and her own; bob's and operator rows (empty owner id) are hidden.
func asAlice(_, _ string, owner meta.Owner) bool {
	if owner.Kind != meta.OwnerUser {
		return true
	}
	return owner.ID == "u-alice"
}

func TestHostKeyList_VisibleFilter(t *testing.T) {
	hostA := meta.NewID()
	svc := &Service{
		Providers: fProviders{},
		Hosts:     fHosts{{Meta: meta.Metadata{ID: hostA, Name: "host-a"}}},
		Models:    fModels{}, Bindings: fBindings{}, Pricings: fPricings{},
		Policies: fPolicies{}, RateLimits: fRLs{},
		HostKeys: fHostKeys{
			{Meta: meta.Metadata{ID: meta.NewID(), Name: "k-alice", Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}}, Spec: hostkey.Spec{HostID: hostA, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "A"}}},
			{Meta: meta.Metadata{ID: meta.NewID(), Name: "k-bob", Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-bob"}}, Spec: hostkey.Spec{HostID: hostA, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "B"}}},
			{Meta: meta.Metadata{ID: meta.NewID(), Name: "k-operator", Owner: meta.Owner{Kind: meta.OwnerUser}}, Spec: hostkey.Spec{HostID: hostA, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "C"}}},
		},
		Visible: asAlice,
	}
	_, keys, err := svc.HostKeyList(context.Background(), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Name != "k-alice" {
		t.Fatalf("keys = %+v, want only k-alice", keys)
	}

	svc.Visible = nil // single-user default: everything visible
	_, keys, err = svc.HostKeyList(context.Background(), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 3 {
		t.Fatalf("unfiltered keys = %d, want 3", len(keys))
	}
}

func TestModelPolicies_VisibleFilter(t *testing.T) {
	svc, _ := fixture()
	// fixture's "tier-1" policy is user-owned with an EMPTY owner id — an
	// operator row, hidden from alice.
	svc.Visible = asAlice
	_, rows, err := svc.ModelPolicies(context.Background(), "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %+v, want operator policy hidden", rows)
	}
}

func TestPolicyProjections_InvisiblePolicyIsNotFound(t *testing.T) {
	svc, _ := fixture()
	svc.Policies = fPolicies{
		{Meta: meta.Metadata{ID: meta.NewID(), Name: "bob-pol", Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-bob"}}, Spec: policy.Spec{}},
		{Meta: meta.Metadata{ID: meta.NewID(), Name: "alice-pol", Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}}, Spec: policy.Spec{}},
	}
	svc.Visible = asAlice

	if _, _, err := svc.PolicyModels(context.Background(), "bob-pol"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PolicyModels(bob-pol) err = %v, want ErrNotFound", err)
	}
	if _, _, err := svc.PolicyHosts(context.Background(), "bob-pol"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PolicyHosts(bob-pol) err = %v, want ErrNotFound", err)
	}
	if _, _, err := svc.PolicyRateLimits(context.Background(), "bob-pol"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PolicyRateLimits(bob-pol) err = %v, want ErrNotFound", err)
	}
	if _, _, err := svc.PolicyModels(context.Background(), "alice-pol"); err != nil {
		t.Fatalf("PolicyModels(alice-pol) err = %v, want nil", err)
	}
}
