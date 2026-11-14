package control

import (
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/meta"
)

// A team admin gets the team and its projects as admin contexts; a developer reached through a group gets that project alone; a viewer gets nothing to create in; the admin token gets every scope and no personal one. Teams come first, then projects, each by slug.
func TestMyContexts(t *testing.T) {
	system := meta.Owner{Kind: meta.OwnerSystem}
	roles, err := role.Builtins()
	if err != nil {
		t.Fatal(err)
	}
	roleID := map[string]string{}
	for _, r := range roles {
		r.Meta.ID = meta.NewID()
		roleID[r.Meta.Name] = r.Meta.ID
	}
	teams := []*team.Team{
		{Meta: meta.Metadata{ID: meta.NewID(), Name: "alpha", DisplayName: "Alpha", Owner: system}},
		{Meta: meta.Metadata{ID: meta.NewID(), Name: "beta", Owner: system}},
	}
	mkProject := func(name string, tm *team.Team) *project.Project {
		return &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerTeam, ID: tm.Meta.ID}}, Spec: project.Spec{TeamID: tm.Meta.ID}}
	}
	web, api, ops := mkProject("web", teams[0]), mkProject("api", teams[1]), mkProject("ops", teams[1])
	bind := func(roleName string, scope meta.Owner, sub rolebinding.Subject) *rolebinding.RoleBinding {
		return &rolebinding.RoleBinding{Meta: meta.Metadata{ID: meta.NewID(), Name: meta.NewID(), Owner: scope}, Spec: rolebinding.Spec{RoleID: roleID[roleName], Scope: scope, Subjects: []rolebinding.Subject{sub}}}
	}
	alphaScope := meta.Owner{Kind: meta.OwnerTeam, ID: teams[0].Meta.ID}
	cat := catalogtest.Catalog{
		Teams: teams, Projects: []*project.Project{web, api, ops}, Roles: roles,
		RoleBindings: []*rolebinding.RoleBinding{
			bind(role.BuiltinTeamAdmin, alphaScope, rolebinding.Subject{Kind: rolebinding.SubjectUser, ID: "u-alice"}),
			bind("developer", meta.Owner{Kind: meta.OwnerProject, ID: api.Meta.ID}, rolebinding.Subject{Kind: rolebinding.SubjectGroup, Name: "eng"}),
			bind("viewer", meta.Owner{Kind: meta.OwnerTeam, ID: teams[1].Meta.ID}, rolebinding.Subject{Kind: rolebinding.SubjectUser, ID: "u-dave"}),
		},
	}.Load(t)
	az := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}

	actors := map[string]*actor.Actor{
		"alice": {UserID: "u-alice", Username: "alice", Subjects: []string{"user:u-alice"}},
		"bob":   {UserID: "u-bob", Username: "bob", Subjects: []string{"user:u-bob", "group:eng"}},
		"dave":  {UserID: "u-dave", Username: "dave", Subjects: []string{"user:u-dave"}},
		"token": {AdminToken: true},
	}
	// A user whose personal account is turned off: rows created there would serve nothing.
	world := &plantest.World{}
	off := world.User()
	off.PersonalAccount = "off"
	actors["off"] = &actor.Actor{UserID: off.ID, Username: "off", Subjects: []string{"user:" + off.ID}}
	mount := func(d Deps) http.Handler {
		r, api := testAPI(t, actors)
		registerMyContexts(api, d, nil)
		return r
	}
	r := mount(Deps{Authz: az, Catalog: cat, PlanView: world.Load(t).Engine.View})

	alpha, beta := teams[0].Meta.ID, teams[1].Meta.ID
	personal := func(id, name string) MyContext {
		return MyContext{Kind: "personal", ID: id, Name: name, DisplayName: name, Admin: true}
	}
	for who, want := range map[string][]MyContext{
		"alice": {personal("u-alice", "alice"), {Kind: "team", ID: alpha, Name: "alpha", DisplayName: "Alpha", Admin: true}, {Kind: "project", ID: web.Meta.ID, Name: "web", DisplayName: "web", TeamID: alpha, Admin: true}},
		"bob":   {personal("u-bob", "bob"), {Kind: "project", ID: api.Meta.ID, Name: "api", DisplayName: "api", TeamID: beta}},
		"dave":  {personal("u-dave", "dave")},
		"off":   {},
		"token": {
			{Kind: "team", ID: alpha, Name: "alpha", DisplayName: "Alpha", Admin: true}, {Kind: "team", ID: beta, Name: "beta", DisplayName: "beta", Admin: true},
			{Kind: "project", ID: api.Meta.ID, Name: "api", DisplayName: "api", TeamID: beta, Admin: true}, {Kind: "project", ID: ops.Meta.ID, Name: "ops", DisplayName: "ops", TeamID: beta, Admin: true},
			{Kind: "project", ID: web.Meta.ID, Name: "web", DisplayName: "web", TeamID: alpha, Admin: true},
		},
	} {
		var got []MyContext
		wantJSON(t, scopeReq(t, r, who, http.MethodGet, "/me/contexts", ""), http.StatusOK, &got)
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("%s (-want +got):\n%s", who, diff)
		}
	}
	if w := scopeReq(t, r, "", http.MethodGet, "/me/contexts", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: status %d, want 401", w.Code)
	}

	wantAPIError(t, scopeReq(t, mount(Deps{Authz: az}), "alice", http.MethodGet, "/me/contexts", ""), http.StatusServiceUnavailable, "", "the catalog is not loaded yet")
}
