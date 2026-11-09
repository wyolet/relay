package control

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/team"
)

// A team admin gets the team and its projects as admin contexts; a developer reached through a group gets that project alone; a viewer gets nothing to create in; the admin token gets every scope and no personal one.
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
	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-Actor", actors))
	registerMyContexts(humachi.New(r, huma.DefaultConfig("contexts-test", "0")), Deps{Authz: az, Catalog: cat}, nil)

	type ctxRow struct {
		kind, name string
		admin      bool
	}
	for who, want := range map[string][]ctxRow{
		"alice": {{"personal", "alice", true}, {"team", "alpha", true}, {"project", "web", true}},
		"bob":   {{"personal", "bob", true}, {"project", "api", false}},
		"dave":  {{"personal", "dave", true}},
		"token": {{"team", "alpha", true}, {"team", "beta", true}, {"project", "api", true}, {"project", "ops", true}, {"project", "web", true}},
	} {
		w := scopeReq(t, r, who, http.MethodGet, "/me/contexts", "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", who, w.Code, w.Body)
		}
		var got []MyContext
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Errorf("%s: contexts %+v, want %+v", who, got, want)
			continue
		}
		for i, c := range got {
			if c.Kind != want[i].kind || c.Name != want[i].name || c.Admin != want[i].admin {
				t.Errorf("%s: context %d = %+v, want %+v", who, i, c, want[i])
			}
			if c.Kind == "project" && c.TeamID == "" {
				t.Errorf("%s: project %s has no teamId", who, c.Name)
			}
		}
		if who == "alice" && (got[1].DisplayName != "Alpha" || got[0].ID != "u-alice") {
			t.Errorf("alice: %+v", got)
		}
	}
	if w := scopeReq(t, r, "", http.MethodGet, "/me/contexts", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: status %d, want 401", w.Code)
	}
}
