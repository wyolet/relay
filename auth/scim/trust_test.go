package scim

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/rbac"
)

const patchMembers = `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"add","path":"members","value":[{"value":"%s"}]}]}`

func (f *fixture) foreignWrites() int {
	n := 0
	for _, w := range f.writes {
		if errors.Is(w.Err, directory.ErrForeign) {
			n++
		}
	}
	return n
}

func TestClientCannotTouchALocalUser(t *testing.T) {
	f := newFixture(t)
	local := &directory.User{Username: "root", Email: "root@example.com"}
	if err := f.dir.CreateUser(context.Background(), nil, local); err != nil {
		t.Fatal(err)
	}
	path := "/Users/" + local.ID
	f.do(http.MethodGet, path, "").want(t, http.StatusNotFound)
	f.do(http.MethodPut, path, `{"userName":"root","active":false}`).want(t, http.StatusNotFound)
	f.do(http.MethodPatch, path, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"replace","path":"active","value":false}]}`).
		want(t, http.StatusNotFound)
	f.do(http.MethodDelete, path, "").want(t, http.StatusNotFound)
	if res := f.do(http.MethodGet, "/Users", "").want(t, http.StatusOK); num(res.body["totalResults"]) != 0 {
		t.Fatalf("a local user is listed: %s", res.raw)
	}
	if u := f.user(local.ID); u.Disabled || u.Username != "root" {
		t.Fatalf("local user changed: %+v", u)
	}
	if f.foreignWrites() != 3 {
		t.Fatalf("writes %+v, want the three refused writes reported as foreign", f.writes)
	}
}

func TestClientCannotTouchAnotherClientsGroup(t *testing.T) {
	f := newFixture(t)
	f.client("okta")
	gid := f.as("okta", http.MethodPost, "/Groups", `{"displayName":"Okta Ops"}`).want(t, http.StatusCreated).str("id")
	mine := f.createUser("ada@example.com", "obj-1")
	f.do(http.MethodGet, "/Groups/"+gid, "").want(t, http.StatusNotFound)
	f.do(http.MethodPatch, "/Groups/"+gid, sprintfMembers(mine)).want(t, http.StatusNotFound)
	f.do(http.MethodDelete, "/Groups/"+gid, "").want(t, http.StatusNotFound)
	if g := f.group(gid); len(g.Spec.MemberIDs) != 0 {
		t.Fatalf("another client's group changed: %v", g.Spec.MemberIDs)
	}
	if f.foreignWrites() != 2 {
		t.Fatalf("writes %+v", f.writes)
	}
	// Nor may okta put entra's user into okta's group.
	f.as("okta", http.MethodPatch, "/Groups/"+gid, sprintfMembers(mine)).want(t, http.StatusBadRequest)
}

func TestClientCannotTouchALocalGroup(t *testing.T) {
	f := newFixture(t)
	local := &directory.Group{Meta: directory.Metadata{Name: "ops"}}
	if err := f.dir.CreateGroup(context.Background(), nil, local); err != nil {
		t.Fatal(err)
	}
	u := f.createUser("ada@example.com", "obj-1")
	f.do(http.MethodPatch, "/Groups/"+local.Meta.ID, sprintfMembers(u)).want(t, http.StatusNotFound)
	f.do(http.MethodPut, "/Groups/"+local.Meta.ID, `{"displayName":"Ops"}`).want(t, http.StatusNotFound)
	if g := f.group(local.Meta.ID); len(g.Spec.MemberIDs) != 0 || g.Spec.External.Source != "" {
		t.Fatalf("local group changed: %+v", g.Spec)
	}
}

func TestClientCannotGrantRolesOrReachBindings(t *testing.T) {
	f := newFixture(t)
	id := f.do(http.MethodPost, "/Users", `{"userName":"ada","roles":[{"value":"admin"}],"password":"hunter2hunter2"}`).
		want(t, http.StatusCreated).str("id")
	f.do(http.MethodPatch, "/Users/"+id, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
		"Operations":[{"op":"add","path":"roles","value":[{"value":"admin"}]},{"op":"replace","value":{"roles":[{"value":"admin"}]}}]}`).
		want(t, http.StatusOK)
	if u := f.user(id); len(u.Roles) != 0 || u.PasswordHash != "" || !u.Identity.IsZero() {
		t.Fatalf("a client set privileges: %+v", u)
	}
	for _, path := range []string{"/Roles", "/RoleBindings", "/Teams", "/Projects", "/ServiceAccounts"} {
		f.do(http.MethodPost, path, `{}`).want(t, http.StatusNotFound)
	}
}

func TestMembershipOfItsOwnGroupSkipsTheGrantCheck(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	gid := f.createGroup("Writers")
	g := f.group(gid)
	role := &directory.Role{Meta: directory.Metadata{Name: "writer"}, Spec: directory.RoleSpec{
		Rules: []rbac.Rule{{Kinds: []string{"documents"}, Verbs: []string{"update"}}}}}
	if err := f.dir.CreateRole(ctx, role); err != nil {
		t.Fatal(err)
	}
	if err := f.dir.CreateRoleBinding(ctx, admin, &directory.RoleBinding{Meta: directory.Metadata{Name: "writers"},
		Spec: directory.RoleBindingSpec{RoleID: role.Meta.ID, Scope: directory.Global,
			Subjects: []directory.Subject{{Kind: directory.SubjectGroup, Name: g.Meta.Name}}}}); err != nil {
		t.Fatal(err)
	}
	u := f.createUser("ada@example.com", "obj-1")
	f.grants.calls = 0
	f.do(http.MethodPatch, "/Groups/"+gid, sprintfMembers(u)).want(t, http.StatusNoContent)
	if f.grants.calls != 0 || !slices.Contains(f.group(gid).Spec.MemberIDs, u) {
		t.Fatalf("grant calls %d, members %v", f.grants.calls, f.group(gid).Spec.MemberIDs)
	}
	// A new group never takes the name a binding already holds.
	other := f.createGroup("Writers")
	if f.group(other).Meta.Name == g.Meta.Name {
		t.Fatal("a second group took a bound name")
	}
}

func sprintfMembers(id string) string {
	return fmt.Sprintf(patchMembers, id)
}
