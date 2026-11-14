package policybinding

import (
	"testing"

	"github.com/wyolet/relay/pkg/meta"
)

const (
	ownerID   = "01a19bc3-0000-7000-8000-000000000001"
	policyID  = "01a19bc3-0000-7000-8000-000000000002"
	accountID = "01a19bc3-0000-7000-8000-000000000003"
	otherID   = "01a19bc3-0000-7000-8000-000000000004"
)

func fix() *PolicyBinding {
	return &PolicyBinding{
		Meta: meta.Metadata{ID: otherID, Name: "ml-eval", Owner: meta.Owner{Kind: meta.OwnerProject, ID: ownerID}},
		Spec: Spec{PolicyID: policyID, AccountIDs: []string{accountID}},
	}
}

func TestScopeKeyAndIsEnabled(t *testing.T) {
	off, on := false, true
	for _, tc := range []struct {
		owner   meta.Owner
		enabled *bool
		key     string
		want    bool
	}{
		{meta.Owner{Kind: meta.OwnerProject, ID: ownerID}, nil, "project:" + ownerID, true},
		{meta.Owner{Kind: meta.OwnerTeam, ID: ownerID}, &on, "team:" + ownerID, true},
		{meta.Owner{Kind: meta.OwnerUser, ID: ownerID}, &off, "user:" + ownerID, false},
		{meta.Owner{Kind: meta.OwnerSystem}, nil, "", true},
	} {
		b := &PolicyBinding{Meta: meta.Metadata{Owner: tc.owner}, Spec: Spec{Enabled: tc.enabled}}
		if b.ScopeKey() != tc.key || b.IsEnabled() != tc.want {
			t.Errorf("%+v: ScopeKey %q IsEnabled %v, want %q %v", tc.owner, b.ScopeKey(), b.IsEnabled(), tc.key, tc.want)
		}
	}
}

func TestValidate(t *testing.T) {
	owner := func(kind meta.OwnerKind, id string) func(*PolicyBinding) {
		return func(b *PolicyBinding) { b.Meta.Owner = meta.Owner{Kind: kind, ID: id} }
	}
	for _, tc := range []struct {
		name string
		edit func(b *PolicyBinding)
		want string
	}{
		{"project owner", func(*PolicyBinding) {}, ""},
		{"team owner", owner(meta.OwnerTeam, ownerID), ""},
		{"user owner", owner(meta.OwnerUser, ownerID), ""},
		{"system owner", owner(meta.OwnerSystem, ""), ""},
		{"several accounts", func(b *PolicyBinding) { b.Spec.AccountIDs = []string{accountID, otherID} }, ""},
		{"system owner with an id", owner(meta.OwnerSystem, ownerID), `policybinding "ml-eval": a system owner has no id, got "01a19bc3-0000-7000-8000-000000000001"`},
		{"team owner without an id", owner(meta.OwnerTeam, ""), `policybinding "ml-eval": a team owner needs its id, got ""`},
		{"user owner without an id", owner(meta.OwnerUser, ""), `policybinding "ml-eval": a user owner needs its id, got ""`},
		{"project owner whose id is no uuid", owner(meta.OwnerProject, "ml"), `policybinding "ml-eval": a project owner needs its id, got "ml"`},
		{"host owner", owner(meta.OwnerHost, ownerID), `policybinding "ml-eval": owner.kind must be system, team, project or user, got "host"`},
		{"no owner", owner("", ""), `policybinding "ml-eval": owner.kind must be system, team, project or user, got ""`},
		{"no accounts", func(b *PolicyBinding) { b.Spec.AccountIDs = nil }, "Key: 'PolicyBinding.Spec.AccountIDs' Error:Field validation for 'AccountIDs' failed on the 'required' tag"},
		{"an empty account list", func(b *PolicyBinding) { b.Spec.AccountIDs = []string{} }, "Key: 'PolicyBinding.Spec.AccountIDs' Error:Field validation for 'AccountIDs' failed on the 'min' tag"},
		{"an account that is not an id", func(b *PolicyBinding) { b.Spec.AccountIDs = []string{accountID, "team-ml"} }, "Key: 'PolicyBinding.Spec.AccountIDs[1]' Error:Field validation for 'AccountIDs[1]' failed on the 'uuid' tag"},
		{"an account twice", func(b *PolicyBinding) { b.Spec.AccountIDs = []string{accountID, otherID, accountID} }, `policybinding "ml-eval": accountIds[2] 01a19bc3-0000-7000-8000-000000000003 is listed twice`},
		{"no policy", func(b *PolicyBinding) { b.Spec.PolicyID = "" }, "Key: 'PolicyBinding.Spec.PolicyID' Error:Field validation for 'PolicyID' failed on the 'required' tag"},
		{"a policy that is not an id", func(b *PolicyBinding) { b.Spec.PolicyID = "default" }, "Key: 'PolicyBinding.Spec.PolicyID' Error:Field validation for 'PolicyID' failed on the 'uuid' tag"},
	} {
		b := fix()
		tc.edit(b)
		got := ""
		if err := b.Validate(); err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %s", tc.name, got, tc.want)
		}
	}
}
