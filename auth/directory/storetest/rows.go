package storetest

import (
	"context"
	"testing"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/directory"
)

func groupMeta(g *directory.Group) *directory.Metadata         { return &g.Meta }
func teamMeta(x *directory.Team) *directory.Metadata           { return &x.Meta }
func projectMeta(p *directory.Project) *directory.Metadata     { return &p.Meta }
func saMeta(sa *directory.ServiceAccount) *directory.Metadata  { return &sa.Meta }
func roleMeta(r *directory.Role) *directory.Metadata           { return &r.Meta }
func bindingMeta(b *directory.RoleBinding) *directory.Metadata { return &b.Meta }
func get[T any](s directory.Store[T], id string) func() (*T, error) {
	return func() (*T, error) { return s.Get(context.Background(), id) }
}

func testRows(t *testing.T, s directory.Stores) {
	f := seed(t, s)

	sameRow(t, "team", fetch(t, get(s.Teams, f.team.Meta.ID)), f.team, teamMeta)
	sameRow(t, "project", fetch(t, get(s.Projects, f.project.Meta.ID)), f.project, projectMeta)
	sameRow(t, "service account", fetch(t, get(s.ServiceAccounts, f.sa.Meta.ID)), f.sa, saMeta)
	sameRow(t, "group", fetch(t, get(s.Groups, f.group.Meta.ID)), f.group, groupMeta)
	sameRow(t, "role", fetch(t, get(s.Roles, f.role.Meta.ID)), f.role, roleMeta)
	sameRow(t, "role binding", fetch(t, get(s.RoleBindings, f.binding.Meta.ID)), f.binding, bindingMeta)

	if f.project.Meta.Owner != (authz.Owner{Kind: directory.OwnerTeam, ID: f.team.Meta.ID}) {
		t.Fatalf("project owner = %+v, want its team", f.project.Meta.Owner)
	}
	if f.sa.Meta.Owner != (authz.Owner{Kind: directory.OwnerProject, ID: f.project.Meta.ID}) {
		t.Fatalf("service account owner = %+v, want its project", f.sa.Meta.Owner)
	}
	if f.binding.Meta.Owner != authz.Owner(f.binding.Spec.Scope) {
		t.Fatalf("binding owner = %+v, want its scope", f.binding.Meta.Owner)
	}

	ctx := context.Background()
	if got, err := s.Teams.Get(ctx, directory.NewID()); err != nil || got != nil {
		t.Fatalf("get of a missing team = %v, %v; want nil, nil", got, err)
	}

	second := &directory.Team{Meta: meta("alpha", directory.OwnerUser, f.user.ID)}
	must(t, s.Teams.Upsert(ctx, second))
	teams, err := s.Teams.List(ctx)
	must(t, err)
	if len(teams) != 2 || teams[0].Meta.Name != "alpha" || teams[1].Meta.Name != "core" {
		t.Fatalf("teams are not ordered by name: %v", names(teams, teamMeta))
	}
	for _, list := range []func() (int, error){
		count(s.Groups), count(s.Projects), count(s.ServiceAccounts), count(s.Roles), count(s.RoleBindings),
	} {
		if n, err := list(); err != nil || n != 1 {
			t.Fatalf("list = %d, %v; want 1 row", n, err)
		}
	}

	clash := &directory.Team{Meta: meta("core", directory.OwnerSystem, "")}
	wantErr(t, "duplicate name", s.Teams.Upsert(ctx, clash), directory.ErrConflict)

	f.group.Spec.MemberIDs = nil
	f.group.Meta.ResourceVersion = ""
	must(t, s.Groups.Upsert(ctx, f.group))
	sameRow(t, "group without members", fetch(t, get(s.Groups, f.group.Meta.ID)), f.group, groupMeta)
}

func fetch[T any](t *testing.T, get func() (*T, error)) *T {
	t.Helper()
	row, err := get()
	must(t, err)
	return row
}

func count[T any](s directory.Store[T]) func() (int, error) {
	return func() (int, error) {
		rows, err := s.List(context.Background())
		return len(rows), err
	}
}

func names[T any](rows []*T, meta func(*T) *directory.Metadata) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = meta(r).Name
	}
	return out
}

func testVersions(t *testing.T, s directory.Stores) {
	ctx := context.Background()
	f := seed(t, s)
	read := fetch(t, get(s.Teams, f.team.Meta.ID))
	first := read.Meta.ResourceVersion

	read.Meta.DisplayName = "Core"
	must(t, s.Teams.Upsert(ctx, read))
	after := fetch(t, get(s.Teams, f.team.Meta.ID))
	if after.Meta.ResourceVersion == first {
		t.Fatalf("version did not change on write: %q", first)
	}

	read.Meta.DisplayName = "Stale"
	wantErr(t, "write at the old version", s.Teams.Upsert(ctx, read), directory.ErrStaleResourceVersion)
	read.Meta.ResourceVersion = "not-a-version"
	wantErr(t, "unparsable version", s.Teams.Upsert(ctx, read), directory.ErrStaleResourceVersion)

	after.Meta.DisplayName = "Current"
	must(t, s.Teams.Upsert(ctx, after))
	read.Meta.ResourceVersion = ""
	read.Meta.DisplayName = "Unconditional"
	must(t, s.Teams.Upsert(ctx, read))
	if got := fetch(t, get(s.Teams, f.team.Meta.ID)); got.Meta.DisplayName != "Unconditional" {
		t.Fatalf("unconditional write lost: %q", got.Meta.DisplayName)
	}

	fresh := &directory.Role{Meta: meta("fresh", directory.OwnerSystem, ""), Spec: f.role.Spec}
	fresh.Meta.ResourceVersion = "7"
	must(t, s.Roles.Upsert(ctx, fresh))
}
