package directory_test

import (
	"slices"
	"testing"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/directory"
)

func TestGroups(t *testing.T) {
	h := newHarness(t)
	ada := &directory.User{Username: "ada"}
	bob := &directory.User{Username: "bob"}
	must(t, h.d.CreateUser(ctx, nil, ada))
	must(t, h.d.CreateUser(ctx, nil, bob))

	g := &directory.Group{Meta: directory.Metadata{DisplayName: "Platform Ops"}}
	must(t, h.d.CreateGroup(ctx, g))
	if g.Meta.Name != "platform-ops" || g.Meta.Owner.Kind != directory.OwnerSystem || !g.IsEnabled() {
		t.Fatalf("created %+v", g.Meta)
	}
	dup := &directory.Group{Meta: directory.Metadata{DisplayName: "Platform Ops"}}
	must(t, h.d.CreateGroup(ctx, dup))
	if dup.Meta.Name != "platform-ops-2" {
		t.Fatalf("second group named %q", dup.Meta.Name)
	}

	must(t, h.d.AddGroupMember(ctx, g.Meta.ID, ada.ID))
	must(t, h.d.AddGroupMember(ctx, g.Meta.ID, bob.ID))
	must(t, h.d.AddGroupMember(ctx, g.Meta.ID, ada.ID))
	got, _ := h.s.Groups.Get(ctx, g.Meta.ID)
	if !slices.Equal(got.Spec.MemberIDs, []string{ada.ID, bob.ID}) {
		t.Fatalf("members = %v", got.Spec.MemberIDs)
	}
	if c := h.notes.last(t); c != (directory.Change{Kind: directory.KindGroup, Op: directory.OpUpsert, ID: g.Meta.ID}) {
		t.Fatalf("change = %+v", c)
	}
	must(t, h.d.RemoveGroupMember(ctx, g.Meta.ID, ada.ID))
	must(t, h.d.RemoveGroupMember(ctx, g.Meta.ID, ada.ID))
	got, _ = h.s.Groups.Get(ctx, g.Meta.ID)
	if !slices.Equal(got.Spec.MemberIDs, []string{bob.ID}) {
		t.Fatalf("members = %v", got.Spec.MemberIDs)
	}
	wantErr(t, "add a missing user", h.d.AddGroupMember(ctx, g.Meta.ID, "nope"), directory.ErrInvalid)
	wantErr(t, "add to a missing group", h.d.AddGroupMember(ctx, "nope", ada.ID), directory.ErrNotFound)

	got.Spec.MemberIDs = []string{ada.ID, bob.ID}
	got.Meta.Name = ""
	must(t, h.d.UpdateGroup(ctx, got))
	if got.Meta.Name != "platform-ops" {
		t.Fatalf("update without a name renamed the group to %q", got.Meta.Name)
	}
	got.Meta.ResourceVersion = "1"
	wantErr(t, "stale update", h.d.UpdateGroup(ctx, got), directory.ErrStaleResourceVersion)

	for name, bad := range map[string]*directory.Group{
		"reserved name":   {Meta: directory.Metadata{Name: "system:everyone"}},
		"invalid name":    {Meta: directory.Metadata{Name: "Bad Name"}},
		"no name at all":  {Meta: directory.Metadata{}},
		"bad owner":       {Meta: directory.Metadata{Name: "x", Owner: authz.Owner{Kind: "team", ID: "t"}}},
		"duplicate":       {Meta: directory.Metadata{Name: "y"}, Spec: directory.GroupSpec{MemberIDs: []string{ada.ID, ada.ID}}},
		"empty member":    {Meta: directory.Metadata{Name: "z"}, Spec: directory.GroupSpec{MemberIDs: []string{""}}},
		"missing members": {Meta: directory.Metadata{Name: "w"}, Spec: directory.GroupSpec{MemberIDs: []string{"nope"}}},
	} {
		wantErr(t, name, h.d.CreateGroup(ctx, bad), directory.ErrInvalid)
	}
	wantErr(t, "update without id", h.d.UpdateGroup(ctx, &directory.Group{}), directory.ErrInvalid)
	wantErr(t, "update a missing group", h.d.UpdateGroup(ctx, &directory.Group{Meta: directory.Metadata{ID: "nope"}}), directory.ErrNotFound)

	must(t, h.d.DeleteGroup(ctx, dup.Meta.ID))
	if c := h.notes.last(t); c.Op != directory.OpDelete || c.ID != dup.Meta.ID {
		t.Fatalf("change = %+v", c)
	}
	wantErr(t, "delete again", h.d.DeleteGroup(ctx, dup.Meta.ID), directory.ErrNotFound)
}

func TestTenancy(t *testing.T) {
	h := newHarness(t)
	team := &directory.Team{Meta: directory.Metadata{Name: "core"}}
	must(t, h.d.CreateTeam(ctx, team))
	if team.Meta.Owner.Kind != directory.OwnerSystem || !team.IsEnabled() {
		t.Fatalf("team %+v", team.Meta)
	}
	p := &directory.Project{Meta: directory.Metadata{Name: "api", Owner: authz.Owner{Kind: "user", ID: "x"}},
		Spec: directory.ProjectSpec{TeamID: team.Meta.ID}}
	must(t, h.d.CreateProject(ctx, p))
	if p.Meta.Owner != (authz.Owner{Kind: directory.OwnerTeam, ID: team.Meta.ID}) || !p.IsEnabled() {
		t.Fatalf("project owner = %+v, want its team", p.Meta.Owner)
	}
	sa := &directory.ServiceAccount{Meta: directory.Metadata{Name: "ci"}, Spec: directory.ServiceAccountSpec{ProjectID: p.Meta.ID}}
	must(t, h.d.CreateServiceAccount(ctx, sa))
	if sa.Meta.Owner != (authz.Owner{Kind: directory.OwnerProject, ID: p.Meta.ID}) || !sa.IsEnabled() {
		t.Fatalf("service account owner = %+v, want its project", sa.Meta.Owner)
	}

	wantErr(t, "project in a missing team", h.d.CreateProject(ctx, &directory.Project{Meta: directory.Metadata{Name: "x"},
		Spec: directory.ProjectSpec{TeamID: "nope"}}), directory.ErrInvalid)
	wantErr(t, "project without a team", h.d.CreateProject(ctx, &directory.Project{Meta: directory.Metadata{Name: "x"}}), directory.ErrInvalid)
	wantErr(t, "account in a missing project", h.d.CreateServiceAccount(ctx, &directory.ServiceAccount{Meta: directory.Metadata{Name: "x"},
		Spec: directory.ServiceAccountSpec{ProjectID: "nope"}}), directory.ErrInvalid)
	wantErr(t, "account without a project", h.d.CreateServiceAccount(ctx, &directory.ServiceAccount{Meta: directory.Metadata{Name: "x"}}), directory.ErrInvalid)
	wantErr(t, "team with a project owner", h.d.CreateTeam(ctx, &directory.Team{Meta: directory.Metadata{Name: "x",
		Owner: authz.Owner{Kind: directory.OwnerProject, ID: p.Meta.ID}}}), directory.ErrInvalid)

	other := &directory.Team{Meta: directory.Metadata{Name: "other"}}
	must(t, h.d.CreateTeam(ctx, other))
	p.Spec.TeamID = other.Meta.ID
	must(t, h.d.UpdateProject(ctx, p))
	if p.Meta.Owner.ID != other.Meta.ID {
		t.Fatalf("moved project owner = %+v", p.Meta.Owner)
	}
	team.Meta.DisplayName = "Core"
	must(t, h.d.UpdateTeam(ctx, team))
	sa.Meta.DisplayName = "CI"
	must(t, h.d.UpdateServiceAccount(ctx, sa))
	wantErr(t, "update a missing team", h.d.UpdateTeam(ctx, &directory.Team{Meta: directory.Metadata{ID: "nope"}}), directory.ErrNotFound)
	wantErr(t, "update a missing project", h.d.UpdateProject(ctx, &directory.Project{Meta: directory.Metadata{ID: "nope"}}), directory.ErrNotFound)
	wantErr(t, "update a missing account", h.d.UpdateServiceAccount(ctx, &directory.ServiceAccount{Meta: directory.Metadata{ID: "nope"}}), directory.ErrNotFound)

	must(t, h.d.DeleteServiceAccount(ctx, sa.Meta.ID))
	must(t, h.d.DeleteTeam(ctx, other.Meta.ID))
	if got, _ := h.s.Projects.Get(ctx, p.Meta.ID); got != nil {
		t.Fatal("team delete left its project")
	}
	wantErr(t, "delete a missing project", h.d.DeleteProject(ctx, p.Meta.ID), directory.ErrNotFound)
	if c := h.notes.last(t); c != (directory.Change{Kind: directory.KindTeam, Op: directory.OpDelete, ID: other.Meta.ID}) {
		t.Fatalf("change = %+v", c)
	}
	p2 := &directory.Project{Meta: directory.Metadata{Name: "web"}, Spec: directory.ProjectSpec{TeamID: team.Meta.ID}}
	must(t, h.d.CreateProject(ctx, p2))
	must(t, h.d.DeleteProject(ctx, p2.Meta.ID))
}
