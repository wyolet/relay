package manifest

import (
	"testing"

	"github.com/wyolet/relay/app/meta"
)

// A manifest may not choose who owns a Team, Group or Role: those rows name a
// scope or a grant, and a user-owned one would hand its author every binding
// already made against that name.
func TestTenancyKindsAreAlwaysSystemOwned(t *testing.T) {
	idx := MapResolver{Users: map[string]string{"mallory": "u-mallory"}}

	teamDoc := TeamDTO{Metadata: WireMeta{Name: "platform"}}
	teamDoc.Metadata.Owner = WireOwner{Kind: meta.OwnerUser, Name: "mallory"}
	tm, err := ToTeam(teamDoc, idx)
	if err != nil {
		t.Fatalf("ToTeam: %v", err)
	}
	if tm.Meta.Owner.Kind != meta.OwnerSystem || tm.Meta.Owner.ID != "" {
		t.Errorf("team owner = %+v, want system", tm.Meta.Owner)
	}

	groupDoc := GroupDTO{Metadata: WireMeta{Name: "platform-eng"}}
	groupDoc.Metadata.Owner = WireOwner{Kind: meta.OwnerUser, Name: "mallory"}
	g, err := ToGroup(groupDoc, idx)
	if err != nil {
		t.Fatalf("ToGroup: %v", err)
	}
	if g.Meta.Owner.Kind != meta.OwnerSystem || g.Meta.Owner.ID != "" {
		t.Errorf("group owner = %+v, want system", g.Meta.Owner)
	}

	roleDoc := RoleDTO{Metadata: WireMeta{Name: "auditor-lite"}}
	roleDoc.Metadata.Owner = WireOwner{Kind: meta.OwnerUser, Name: "mallory"}
	roleDoc.Spec.Rules = []RoleRuleDTO{{Kinds: []string{"usage"}, Verbs: []string{"read"}}}
	r, err := ToRole(roleDoc, idx)
	if err != nil {
		t.Fatalf("ToRole: %v", err)
	}
	if r.Meta.Owner.Kind != meta.OwnerSystem || r.Meta.Owner.ID != "" {
		t.Errorf("role owner = %+v, want system", r.Meta.Owner)
	}
}

// A user-owned row names its owner by username on the wire like every other
// cross-reference; leaving the username in place would make the stored owner
// unresolvable.
func TestUserOwnerNameResolvesToID(t *testing.T) {
	idx := MapResolver{
		Policies: map[string]string{"basic": "p-1"},
		Users:    map[string]string{"alice": "u-alice"},
	}
	d := KeyDTO{Metadata: WireMeta{Name: "alice-cli"}}
	d.Metadata.Owner = WireOwner{Kind: meta.OwnerUser, Name: "alice"}
	d.Spec.Policy = "basic"
	d.Spec.Principal = PrincipalDTO{Kind: "user", Name: "alice"}

	k, err := ToKey(d, idx)
	if err != nil {
		t.Fatalf("ToKey: %v", err)
	}
	if k.Meta.Owner.ID != "u-alice" {
		t.Errorf("owner id = %q, want the resolved user id", k.Meta.Owner.ID)
	}
}

// A Key resolves its policy through its principal when it declares none, so
// omitting spec.policy is legal on the wire.
func TestKeyPolicyIsOptional(t *testing.T) {
	idx := MapResolver{ServiceAccounts: map[string]string{"ci": "sa-1"}}
	d := KeyDTO{Metadata: WireMeta{Name: "ci-key"}}
	d.Spec.Principal = PrincipalDTO{Kind: "serviceaccount", Name: "ci"}

	k, err := ToKey(d, idx)
	if err != nil {
		t.Fatalf("ToKey with no policy: %v", err)
	}
	if k.Spec.PolicyID != "" {
		t.Errorf("policy id = %q, want empty", k.Spec.PolicyID)
	}

	d.Spec.Policy = "missing"
	if _, err := ToKey(d, idx); err == nil {
		t.Error("a named policy that does not resolve must still be an error")
	}
}

// A budget with no period is a monthly budget; without the default the
// stored row and the manifest that declared it never converge.
func TestBudgetPeriodDefaultsToMonth(t *testing.T) {
	d := TeamDTO{Metadata: WireMeta{Name: "platform"}}
	d.Spec.Budget = &BudgetDTO{Amount: "100"}
	tm, err := ToTeam(d, MapResolver{})
	if err != nil {
		t.Fatalf("ToTeam: %v", err)
	}
	if tm.Spec.Budget.Period != "month" {
		t.Errorf("period = %q, want month", tm.Spec.Budget.Period)
	}

	d.Spec.Budget.Period = "week"
	tm, err = ToTeam(d, MapResolver{})
	if err != nil {
		t.Fatalf("ToTeam: %v", err)
	}
	if tm.Spec.Budget.Period != "week" {
		t.Errorf("period = %q, want the declared week", tm.Spec.Budget.Period)
	}
}
