package storetest

import (
	"context"
	"reflect"
	"testing"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/rbac"
)

func testReferences(t *testing.T, s directory.Stores) {
	ctx := context.Background()
	f := seed(t, s)
	missing := directory.NewID()

	p := &directory.Project{Meta: meta("orphan", "", ""), Spec: directory.ProjectSpec{TeamID: missing}}
	wantErr(t, "project in a missing team", s.Projects.Upsert(ctx, p), directory.ErrInvalid)
	sa := &directory.ServiceAccount{Meta: meta("orphan", "", ""), Spec: directory.ServiceAccountSpec{ProjectID: missing}}
	wantErr(t, "service account in a missing project", s.ServiceAccounts.Upsert(ctx, sa), directory.ErrInvalid)
	g := &directory.Group{Meta: meta("ghosts", directory.OwnerSystem, ""), Spec: directory.GroupSpec{MemberIDs: []string{missing}}}
	wantErr(t, "group with a missing member", s.Groups.Upsert(ctx, g), directory.ErrInvalid)

	bind := func(roleID string, sub directory.Subject) *directory.RoleBinding {
		return &directory.RoleBinding{Meta: meta("dangling", "", ""), Spec: directory.RoleBindingSpec{
			RoleID: roleID, Scope: directory.Global, Subjects: []directory.Subject{sub}}}
	}
	wantErr(t, "binding of a missing role",
		s.RoleBindings.Upsert(ctx, bind(missing, directory.Subject{Kind: directory.SubjectUser, ID: f.user.ID})), directory.ErrInvalid)
	wantErr(t, "binding to a missing user",
		s.RoleBindings.Upsert(ctx, bind(f.role.Meta.ID, directory.Subject{Kind: directory.SubjectUser, ID: missing})), directory.ErrInvalid)
	wantErr(t, "binding to a missing service account",
		s.RoleBindings.Upsert(ctx, bind(f.role.Meta.ID, directory.Subject{Kind: directory.SubjectServiceAccount, ID: missing})), directory.ErrInvalid)
	must(t, s.RoleBindings.Upsert(ctx, bind(f.role.Meta.ID, directory.Subject{Kind: directory.SubjectGroup, Name: "idp-only"})))

	if got := fetch(t, get(s.RoleBindings, f.binding.Meta.ID)); len(got.Spec.Subjects) != 4 {
		t.Fatalf("a failed write changed another binding: %+v", got.Spec.Subjects)
	}
}

func testCascades(t *testing.T, s directory.Stores) {
	ctx := context.Background()
	f := seed(t, s)

	must(t, s.ServiceAccounts.Delete(ctx, f.sa.Meta.ID))
	b := fetch(t, get(s.RoleBindings, f.binding.Meta.ID))
	wantSubjects := []directory.Subject{
		{Kind: directory.SubjectUser, ID: f.user.ID},
		{Kind: directory.SubjectGroup, Name: "ops"},
		directory.AllAuthenticated,
	}
	if !reflect.DeepEqual(b.Spec.Subjects, wantSubjects) {
		t.Fatalf("subjects after the service account went = %+v", b.Spec.Subjects)
	}

	must(t, s.Users.Delete(ctx, f.user.ID))
	if g := fetch(t, get(s.Groups, f.group.Meta.ID)); g.Spec.MemberIDs != nil {
		t.Fatalf("deleted user still a member: %v", g.Spec.MemberIDs)
	}
	b = fetch(t, get(s.RoleBindings, f.binding.Meta.ID))
	if !reflect.DeepEqual(b.Spec.Subjects, wantSubjects[1:]) {
		t.Fatalf("subjects after the user went = %+v", b.Spec.Subjects)
	}

	sa := &directory.ServiceAccount{Meta: meta("deploy", "", ""), Spec: directory.ServiceAccountSpec{ProjectID: f.project.Meta.ID}}
	must(t, s.ServiceAccounts.Upsert(ctx, sa))
	must(t, s.Teams.Delete(ctx, f.team.Meta.ID))
	if p := fetch(t, get(s.Projects, f.project.Meta.ID)); p != nil {
		t.Fatal("team delete left its project")
	}
	if got := fetch(t, get(s.ServiceAccounts, sa.Meta.ID)); got != nil {
		t.Fatal("team delete left a service account of its project")
	}

	other := &directory.RoleBinding{Meta: meta("everyone", "", ""), Spec: directory.RoleBindingSpec{
		RoleID: f.role.Meta.ID, Scope: rbac.Scope{Kind: "folder", ID: "f-1"}, Subjects: []directory.Subject{directory.AllAuthenticated}}}
	must(t, s.RoleBindings.Upsert(ctx, other))
	must(t, s.Roles.Delete(ctx, f.role.Meta.ID))
	for _, id := range []string{f.binding.Meta.ID, other.Meta.ID} {
		if got := fetch(t, get(s.RoleBindings, id)); got != nil {
			t.Fatalf("role delete left binding %s", got.Meta.Name)
		}
	}
	must(t, s.Roles.Delete(ctx, f.role.Meta.ID))
}
