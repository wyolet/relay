package apply

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/team"
)

// scopedTeamAuthz grants a fixed set of actions at one team and nothing
// anywhere else, standing in for one scoped role binding.
type scopedTeamAuthz struct {
	teamID  string
	actions map[string]bool
}

func (s scopedTeamAuthz) Authorize(_ context.Context, action string, res authz.Resource) error {
	if res.Owner != nil && res.Owner.Kind == meta.OwnerTeam && res.Owner.ID == s.teamID && s.actions[action] {
		return nil
	}
	return authz.ErrForbidden
}

// A bundle cannot hand out more than its author holds: the escalation rule
// applies to a declared RoleBinding exactly as it does to a POST.
func TestApplyRefusesAnEscalatingRoleBinding(t *testing.T) {
	wide := &role.Role{
		Meta: meta.Metadata{ID: "r-wide", Name: "admin", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: role.Spec{Rules: []role.Rule{{Kinds: []string{"*"}, Verbs: []string{"*"}}}},
	}
	narrow := &role.Role{
		Meta: meta.Metadata{ID: "r-narrow", Name: "team-reader", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: role.Spec{Rules: []role.Rule{{Kinds: []string{"keys"}, Verbs: []string{"get"}}}},
	}
	rows := &Rows{
		Roles: []*role.Role{wide, narrow},
		Teams: []*team.Team{{Meta: meta.Metadata{ID: "t-1"}}, {Meta: meta.Metadata{ID: "t-2"}}},
	}
	holder := scopedTeamAuthz{teamID: "t-1", actions: map[string]bool{"keys.get": true}}
	b := &builder{rows: rows, opts: Options{Authz: holder}}
	check := refsFor(b, refcheck.Checker.RoleBinding)

	binding := func(roleID, teamID string) *rolebinding.RoleBinding {
		rb := &rolebinding.RoleBinding{Meta: meta.Metadata{Name: "rb"}}
		rb.Spec.RoleID = roleID
		rb.Spec.Scope = meta.Owner{Kind: meta.OwnerTeam, ID: teamID}
		return rb
	}
	if err := check(context.Background(), binding(narrow.Meta.ID, "t-1")); err != nil {
		t.Fatalf("binding a role the author holds at their own team: %v", err)
	}
	if err := check(context.Background(), binding(wide.Meta.ID, "t-1")); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("binding a wildcard role = %v, want forbidden", err)
	}
	if err := check(context.Background(), binding(narrow.Meta.ID, "t-2")); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("binding at a foreign team = %v, want forbidden", err)
	}

	// A loader with no authorizer (the boot seed) writes what it is given.
	seed := &builder{rows: rows}
	if refsFor(seed, refcheck.Checker.RoleBinding) != nil {
		t.Fatal("boot seed has a grant gate, want none")
	}
}
