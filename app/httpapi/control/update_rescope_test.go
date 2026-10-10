package control

import (
	"context"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
)

// rescopeFixture is team T with projects P1 and P2. The caller is
// project-admin at P1 and holds secondRole at P2; P1 also holds one role
// binding and one service account the caller may update.
type rescopeFixture struct {
	h            http.Handler
	p1, p2       meta.Owner
	roleID       map[string]string
	victimID     string
	rbAtP1       *rolebinding.RoleBinding
	saAtP1       *serviceaccount.ServiceAccount
	roleBindings *memStore[rolebinding.RoleBinding]
	accounts     *memStore[serviceaccount.ServiceAccount]
}

func newRescopeFixture(t *testing.T, secondRole string) *rescopeFixture {
	t.Helper()
	yes := true
	teamID, p1ID, p2ID := meta.NewID(), meta.NewID(), meta.NewID()
	callerID, victimID := meta.NewID(), meta.NewID()

	builtins, err := role.Builtins()
	if err != nil {
		t.Fatal(err)
	}
	roleID := map[string]string{}
	rolesByID := map[string]*role.Role{}
	for _, r := range builtins {
		r.Meta.ID = meta.NewID()
		r.Spec.Enabled = &yes
		roleID[r.Meta.Name] = r.Meta.ID
		rolesByID[r.Meta.ID] = r
	}

	tm := &team.Team{Meta: meta.Metadata{ID: teamID, Name: "t", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	tm.Spec.Enabled = &yes
	mkProject := func(id, name string) *project.Project {
		p := &project.Project{Meta: meta.Metadata{ID: id, Name: name}}
		p.Spec.TeamID = teamID
		p.Spec.Enabled = &yes
		p.StampOwner()
		return p
	}
	projects := map[string]*project.Project{p1ID: mkProject(p1ID, "p1"), p2ID: mkProject(p2ID, "p2")}

	p1 := meta.Owner{Kind: meta.OwnerProject, ID: p1ID}
	p2 := meta.Owner{Kind: meta.OwnerProject, ID: p2ID}
	mkRB := func(name, rID string, scope meta.Owner, subject string) *rolebinding.RoleBinding {
		b := &rolebinding.RoleBinding{Meta: meta.Metadata{ID: meta.NewID(), Name: name}}
		b.Spec.RoleID = rID
		b.Spec.Scope = scope
		b.Spec.Subjects = []rolebinding.Subject{{Kind: rolebinding.SubjectUser, ID: subject}}
		b.Spec.Enabled = &yes
		b.StampOwner()
		return b
	}
	adminAtP1 := mkRB("caller-p1-admin", roleID["project-admin"], p1, callerID)
	secondAtP2 := mkRB("caller-p2", roleID[secondRole], p2, callerID)
	rbAtP1 := mkRB("viewer-p1", roleID["viewer"], p1, victimID)

	rbmeta := func(b *rolebinding.RoleBinding) *meta.Metadata { return &b.Meta }
	roleBindings := &memStore[rolebinding.RoleBinding]{metaOf: rbmeta, items: map[string]*rolebinding.RoleBinding{
		adminAtP1.Meta.ID: adminAtP1, secondAtP2.Meta.ID: secondAtP2, rbAtP1.Meta.ID: rbAtP1,
	}}
	sa := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "bot"}}
	sa.Spec.ProjectID = p1ID
	sa.StampOwner()
	sameta := func(s *serviceaccount.ServiceAccount) *meta.Metadata { return &s.Meta }
	accounts := &memStore[serviceaccount.ServiceAccount]{metaOf: sameta, items: map[string]*serviceaccount.ServiceAccount{sa.Meta.ID: sa}}

	cat := catalogtest.Catalog{}.New()
	cat.UseTenancy(catalogtest.Rows[team.Team]{tm}, catalogtest.Rows[project.Project]{projects[p1ID], projects[p2ID]},
		accounts, catalogtest.Rows[group.Group](nil),
		catalogtest.Rows[role.Role](builtins), roleBindings, catalogtest.Rows[policybinding.PolicyBinding](nil))
	if err := cat.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	rbac := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
	checker := refcheck.Checker{Authz: rbac, Rows: refcheck.Lookup{
		Role:    func(_ context.Context, id string) *role.Role { return rolesByID[id] },
		Project: func(_ context.Context, id string) *project.Project { return projects[id] },
		Team: func(_ context.Context, id string) *team.Team {
			if id == teamID {
				return tm
			}
			return nil
		},
	}}

	caller := &actor.Actor{UserID: callerID, Username: "caller", Subjects: appcatalog.UserSubjects(callerID, nil, nil)}
	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-Actor", map[string]*actor.Actor{"caller": caller}))
	cfg := huma.DefaultConfig("rescope-test", "0")
	cfg.Components.Schemas = httpapi.NewRegistry()
	api := humachi.New(r, cfg)
	registerKind[rolebinding.RoleBinding](
		api, "role-bindings", "role-binding", roleBindings, rbac, rbmeta,
		func(b *rolebinding.RoleBinding) error { b.StampOwner(); return b.Validate() },
		"",
		listScanResolver[rolebinding.RoleBinding](roleBindings, rbmeta),
		func(ctx context.Context, action string, _, incoming *rolebinding.RoleBinding) error {
			if action == "delete" || incoming == nil {
				return nil
			}
			return refErr(checker.RoleBinding(ctx, incoming))
		},
		nil, nil, nil, nil, noSettings{}, false, nil, nil,
	)
	registerKind[serviceaccount.ServiceAccount](
		api, "service-accounts", "service-account", accounts, rbac, sameta,
		func(s *serviceaccount.ServiceAccount) error { return s.Validate() },
		meta.OwnerProject,
		listScanResolver[serviceaccount.ServiceAccount](accounts, sameta),
		func(ctx context.Context, action string, _, incoming *serviceaccount.ServiceAccount) error {
			if action == "delete" || incoming == nil {
				return nil
			}
			return refErr(checker.ServiceAccount(ctx, incoming))
		},
		nil, nil, nil, nil, noSettings{}, false, nil, nil,
	)
	return &rescopeFixture{h: r, p1: p1, p2: p2, roleID: roleID, victimID: victimID,
		rbAtP1: rbAtP1, saAtP1: sa, roleBindings: roleBindings, accounts: accounts}
}

func (f *rescopeFixture) roleBindingAtP2(roleName string) string {
	return `{"metadata":{"name":"viewer-p1","displayName":"Moved"},` +
		`"spec":{"roleId":"` + f.roleID[roleName] + `",` +
		`"scope":{"kind":"project","id":"` + f.p2.ID + `"},` +
		`"subjects":[{"kind":"user","id":"` + f.victimID + `"}]}}`
}

func (f *rescopeFixture) serviceAccountAtP2() string {
	return `{"metadata":{"name":"bot","displayName":"Bot"},"spec":{"projectId":"` + f.p2.ID + `"}}`
}

// A caller who may not create role bindings at P2 cannot get one there by
// editing a binding they may update at P1.
func TestUpdateRoleBindingIntoScopeWithoutCreateIsRefused(t *testing.T) {
	f := newRescopeFixture(t, "developer")
	if w := scopeReq(t, f.h, "caller", http.MethodPost, "/role-bindings", f.roleBindingAtP2("developer")); w.Code != http.StatusForbidden {
		t.Fatalf("create at P2 = %d, want 403: %s", w.Code, w.Body)
	}
	w := scopeReq(t, f.h, "caller", http.MethodPut, "/role-bindings/by-id/"+f.rbAtP1.Meta.ID, f.roleBindingAtP2("developer"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("move to P2 = %d, want 403: %s", w.Code, w.Body)
	}
	if got := f.roleBindings.items[f.rbAtP1.Meta.ID]; got.Spec.Scope != f.p1 || got.Meta.Owner != f.p1 {
		t.Fatalf("stored binding moved to %+v (owner %+v)", got.Spec.Scope, got.Meta.Owner)
	}
}

func TestUpdateRoleBindingIntoScopeWithCreateSucceeds(t *testing.T) {
	f := newRescopeFixture(t, "project-admin")
	w := scopeReq(t, f.h, "caller", http.MethodPut, "/role-bindings/by-id/"+f.rbAtP1.Meta.ID, f.roleBindingAtP2("project-admin"))
	if w.Code != http.StatusOK {
		t.Fatalf("move to P2 = %d, want 200: %s", w.Code, w.Body)
	}
	if got := f.roleBindings.items[f.rbAtP1.Meta.ID]; got.Spec.Scope != f.p2 || got.Meta.Owner != f.p2 {
		t.Fatalf("stored binding at %+v (owner %+v), want P2", got.Spec.Scope, got.Meta.Owner)
	}
}

// A service account's owner is checked against spec.projectId before the
// guard re-derives it, so PUT cannot move one even with create at both ends.
func TestUpdateServiceAccountCannotChangeProject(t *testing.T) {
	f := newRescopeFixture(t, "developer")
	w := scopeReq(t, f.h, "caller", http.MethodPut, "/service-accounts/by-id/"+f.saAtP1.Meta.ID, f.serviceAccountAtP2())
	if w.Code < 400 {
		t.Fatalf("move to P2 = %d, want refused: %s", w.Code, w.Body)
	}
	if got := f.accounts.items[f.saAtP1.Meta.ID]; got.Meta.Owner != f.p1 {
		t.Fatalf("stored account moved to %+v", got.Meta.Owner)
	}
}
