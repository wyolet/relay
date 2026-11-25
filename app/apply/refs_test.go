package apply

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/meta"
)

// A key naming an account held at another host is refused in every load,
// the boot seed included, with the message the API's guard uses.
func TestApplyRejectsAHostKeyOnAnotherHostsAccount(t *testing.T) {
	h1 := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "openai"}}
	h2 := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "anthropic"}}
	acct := &account.Account{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-openai", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: account.Spec{HostID: h1.Meta.ID}}
	rows := &Rows{Hosts: []*host.Host{h1, h2}, Accounts: []*account.Account{acct}}
	check := (&builder{rows: rows}).refs().HostKey

	ok := &hostkey.HostKey{Meta: meta.Metadata{Name: "k", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: hostkey.Spec{HostID: h1.Meta.ID, AccountID: acct.Meta.ID}}
	if err := check(context.Background(), ok); err != nil {
		t.Fatalf("account at the key's own host: %v", err)
	}
	other := &hostkey.HostKey{Meta: meta.Metadata{Name: "k", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: hostkey.Spec{HostID: h2.Meta.ID, AccountID: acct.Meta.ID}}
	if err := check(context.Background(), other); err == nil || !strings.Contains(err.Error(), "held at another host") {
		t.Fatalf("account at another host: err = %v", err)
	}
	unknown := &hostkey.HostKey{Meta: meta.Metadata{Name: "k", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: hostkey.Spec{HostID: h1.Meta.ID, AccountID: meta.NewID()}}
	if err := check(context.Background(), unknown); err == nil {
		t.Error("unresolvable account accepted")
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

// Without an authorizer (the seed, the CLI) apply still keeps a binding to accounts inside its owner's scope: a project binds its own service account's account and not another project's.
func TestApplyKeepsBindingAccountsInsideTheOwnersScope(t *testing.T) {
	rows := tenancyRows(false)
	other := &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: "other"}, Spec: project.Spec{TeamID: rows.Teams[0].Meta.ID}}
	other.StampOwner()
	rows.Projects = append(rows.Projects, other)
	sa := rows.ServiceAccounts[0]
	acct := account.ForServiceAccount(sa.Meta.ID, sa.Meta.Name, "", sa.Meta.Owner)
	acct.Meta.ID = meta.NewID()
	rows.Accounts = []*account.Account{acct}
	rows.Policies = []*policy.Policy{{Meta: meta.Metadata{ID: meta.NewID(), Name: "shared-policy", Owner: meta.Owner{Kind: meta.OwnerSystem}}}}
	plan := func(project string) error {
		b := &builder{opts: Options{Stores: &Stores{}}, rows: rows, idx: newIndex(rows)}
		doc := "kind: PolicyBinding\nmetadata: {name: indexer-policy, owner: {kind: project, name: " + project + "}}\n" +
			"spec: {policy: shared-policy, accounts: [" + acct.Meta.Name + "]}\n"
		return b.run(context.Background(), userDocs(t, docHeader+doc))
	}
	if err := plan("ml-search"); err != nil {
		t.Fatalf("the account's own project binding it: %v", err)
	}
	if err := plan("other"); err == nil || !strings.Contains(err.Error(), "outside the scope") {
		t.Fatalf("another project binding the account = %v, want a refusal", err)
	}
}
