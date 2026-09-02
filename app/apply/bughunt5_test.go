package apply

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
)

// A key whose tier policy belongs to another host is dropped from the
// snapshot, so apply refuses it with the message the API's guard uses.
func TestApplyRejectsAHostKeyOnAForeignPolicy(t *testing.T) {
	pols := map[string]*policy.Policy{
		"p-1": {Meta: meta.Metadata{ID: "p-1", Name: "openai-tier", Owner: meta.Owner{Kind: meta.OwnerHost, ID: "h-1"}}},
		"p-2": {Meta: meta.Metadata{ID: "p-2", Name: "team-pol", Owner: meta.Owner{Kind: meta.OwnerProject, ID: "proj-1"}}},
	}
	check := checkHostKeyPolicy(pols)
	ok := &hostkey.HostKey{Meta: meta.Metadata{Name: "k"}, Spec: hostkey.Spec{HostID: "h-1", PolicyID: "p-1"}}
	if err := check(context.Background(), ok); err != nil {
		t.Fatalf("host-owned policy of the key's own host: %v", err)
	}
	foreign := &hostkey.HostKey{Meta: meta.Metadata{Name: "k"}, Spec: hostkey.Spec{HostID: "h-1", PolicyID: "p-2"}}
	err := check(context.Background(), foreign)
	if err == nil {
		t.Fatal("a project-owned policy was accepted for a host key")
	}
	if got := err.Error(); got != `policy "team-pol" is not host-owned by host "h-1" (owner=project/proj-1)` {
		t.Errorf("message = %q", got)
	}
	// Another host's tier policy is refused too.
	otherHost := &hostkey.HostKey{Meta: meta.Metadata{Name: "k"}, Spec: hostkey.Spec{HostID: "h-2", PolicyID: "p-1"}}
	if err := check(context.Background(), otherHost); err == nil {
		t.Error("a policy owned by a different host was accepted")
	}
	// A policy this run cannot resolve is left to the snapshot's own drop.
	unknown := &hostkey.HostKey{Meta: meta.Metadata{Name: "k"}, Spec: hostkey.Spec{HostID: "h-1", PolicyID: "p-9"}}
	if err := check(context.Background(), unknown); err != nil {
		t.Errorf("unresolvable policy = %v, want it left alone", err)
	}
}

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
	roles := map[string]*role.Role{wide.Meta.ID: wide, narrow.Meta.ID: narrow}
	holder := scopedTeamAuthz{teamID: "t-1", actions: map[string]bool{"keys.get": true}}
	b := &builder{rows: &Rows{}, opts: Options{Authz: holder}}
	check := b.checkRoleBindingGrant(roles)

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
	seed := &builder{rows: &Rows{}}
	if err := seed.checkRoleBindingGrant(roles)(context.Background(), binding(wide.Meta.ID, "t-1")); err != nil {
		t.Fatalf("boot seed = %v, want no gate", err)
	}
}
