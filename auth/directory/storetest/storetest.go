// Package storetest is the behaviour every directory.Stores implementation must share: lookups, unique names, resource versions, references and delete cascades. A store's own tests call Run with a constructor that returns empty stores.
//
// Out of scope: anything a particular store adds, such as its encoding or schema.
package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/rbac"
)

// Run runs the suite; open must return empty stores, a fresh set per call.
func Run(t *testing.T, open func(t *testing.T) directory.Stores) {
	t.Run("users", func(t *testing.T) { testUsers(t, open(t)) })
	t.Run("rows", func(t *testing.T) { testRows(t, open(t)) })
	t.Run("versions", func(t *testing.T) { testVersions(t, open(t)) })
	t.Run("references", func(t *testing.T) { testReferences(t, open(t)) })
	t.Run("cascades", func(t *testing.T) { testCascades(t, open(t)) })
}

// fixture is one row of every kind, linked to each other.
type fixture struct {
	user    *directory.User
	team    *directory.Team
	project *directory.Project
	sa      *directory.ServiceAccount
	group   *directory.Group
	role    *directory.Role
	binding *directory.RoleBinding
}

func seed(t *testing.T, s directory.Stores) fixture {
	t.Helper()
	ctx := context.Background()
	no := false
	f := fixture{
		user: &directory.User{ID: directory.NewID(), Username: "ada", Email: "ada@example.com",
			Identity: directory.Identity{Issuer: "https://idp.example.com", Subject: "sub-1"}, Roles: []string{"viewer"}},
		team: &directory.Team{Meta: meta("core", directory.OwnerSystem, ""),
			Spec: directory.TeamSpec{Attributes: map[string]json.RawMessage{"budget": json.RawMessage(`{"amount":"10.00","period":"month"}`)}}},
	}
	f.team.Meta.Description = "the core team"
	f.team.Meta.Labels = map[string]string{"tier": "gold"}
	f.team.Meta.Annotations = map[string]string{"note": "seeded"}
	f.team.Meta.Dirty = true
	f.project = &directory.Project{Meta: meta("api", "", ""), Spec: directory.ProjectSpec{TeamID: f.team.Meta.ID, Enabled: &no}}
	f.sa = &directory.ServiceAccount{Meta: meta("ci", "", ""), Spec: directory.ServiceAccountSpec{ProjectID: f.project.Meta.ID,
		Attributes: map[string]json.RawMessage{"policyId": json.RawMessage(`"p-1"`)}}}
	f.group = &directory.Group{Meta: meta("ops", directory.OwnerUser, f.user.ID), Spec: directory.GroupSpec{MemberIDs: []string{f.user.ID}}}
	f.role = &directory.Role{Meta: meta("reader", directory.OwnerSystem, ""), Spec: directory.RoleSpec{
		Rules: []rbac.Rule{{Kinds: []string{"documents"}, Verbs: []string{"get", "list"}}}}}
	f.binding = &directory.RoleBinding{Meta: meta("readers", "", ""), Spec: directory.RoleBindingSpec{
		RoleID: f.role.Meta.ID,
		Scope:  rbac.Scope{Kind: directory.OwnerProject, ID: f.project.Meta.ID},
		Subjects: []directory.Subject{
			{Kind: directory.SubjectUser, ID: f.user.ID},
			{Kind: directory.SubjectServiceAccount, ID: f.sa.Meta.ID},
			{Kind: directory.SubjectGroup, Name: "ops"},
			directory.AllAuthenticated,
		}}}
	must(t, s.Users.Upsert(ctx, f.user))
	must(t, s.Teams.Upsert(ctx, f.team))
	must(t, s.Projects.Upsert(ctx, f.project))
	must(t, s.ServiceAccounts.Upsert(ctx, f.sa))
	must(t, s.Groups.Upsert(ctx, f.group))
	must(t, s.Roles.Upsert(ctx, f.role))
	must(t, s.RoleBindings.Upsert(ctx, f.binding))
	return f
}

func meta(name, ownerKind, ownerID string) directory.Metadata {
	return directory.Metadata{ID: directory.NewID(), Name: name, DisplayName: name, Owner: authz.Owner{Kind: ownerKind, ID: ownerID}}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: err = %v, want %v", what, err, want)
	}
}

// sameRow compares two rows of one kind, ignoring the fields the store sets.
func sameRow[T any](t *testing.T, what string, got, want *T, meta func(*T) *directory.Metadata) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: not found", what)
	}
	g, w := *got, *want
	clearStoreFields(meta(&g))
	clearStoreFields(meta(&w))
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("%s:\n got  %+v\n want %+v", what, g, w)
	}
	if m := meta(got); m.ResourceVersion == "" || m.CreatedAt.IsZero() || m.UpdatedAt.IsZero() {
		t.Fatalf("%s: store did not set version and timestamps: %+v", what, m)
	}
}

func clearStoreFields(m *directory.Metadata) {
	m.ResourceVersion, m.CreatedAt, m.UpdatedAt = "", time.Time{}, time.Time{}
}
