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
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/user"
)

// bindingSnapshot answers the three questions the RBAC evaluator asks, with
// one role granting users.list at one scope.
type bindingSnapshot struct {
	scope meta.Owner
	role  *role.Role
}

func (s bindingSnapshot) ScopeChainFor(kind, _ string, o *meta.Owner) []meta.Owner {
	chain := []meta.Owner{}
	if o != nil && o.Kind != meta.OwnerSystem {
		chain = append(chain, *o)
	}
	return append(chain, meta.Owner{Kind: meta.OwnerSystem})
}

func (s bindingSnapshot) RoleBindingsForSubject(subject string) []*rolebinding.RoleBinding {
	if subject != "user:u-alice" {
		return nil
	}
	return []*rolebinding.RoleBinding{{
		Meta: meta.Metadata{ID: "rb-1", Name: "alice-list"},
		Spec: rolebinding.Spec{RoleID: s.role.Meta.ID, Scope: s.scope},
	}}
}

func (s bindingSnapshot) ProjectsInTeam(string) []*project.Project { return nil }

func (s bindingSnapshot) Role(id string) (*role.Role, bool) {
	if id != s.role.Meta.ID {
		return nil, false
	}
	return s.role, true
}

// GET /users lists every account in the deployment, so it is a global read:
// a binding inside one project is not a grant to enumerate everyone.
func TestUsersListNeedsAGlobalBinding(t *testing.T) {
	lister := &role.Role{
		Meta: meta.Metadata{ID: "r-1", Name: "user-lister", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: role.Spec{Rules: []role.Rule{{Kinds: []string{"users"}, Verbs: []string{"list"}}}},
	}
	scoped := bindingSnapshot{scope: meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}, role: lister}
	global := bindingSnapshot{scope: meta.Owner{Kind: meta.OwnerSystem}, role: lister}

	res := authz.Resource{Kind: "user", Owner: &meta.Owner{Kind: meta.OwnerSystem}}
	ctx := actor.WithActor(context.Background(), &actor.Actor{
		UserID: "u-alice", Username: "alice", Subjects: []string{"user:u-alice"},
	})

	scopedAuthz := authz.RBAC{Snap: func() authz.Snapshot { return scoped }}
	if err := scopedAuthz.Authorize(ctx, "users.list", res); err == nil {
		t.Error("a project-scoped binding must not grant the global user list")
	}
	globalAuthz := authz.RBAC{Snap: func() authz.Snapshot { return global }}
	if err := globalAuthz.Authorize(ctx, "users.list", res); err != nil {
		t.Errorf("a global binding must grant the user list: %v", err)
	}
}

// captureAuthz records the resource the handler authorizes against.
type captureAuthz struct{ got authz.Resource }

func (c *captureAuthz) Authorize(_ context.Context, _ string, res authz.Resource) error {
	c.got = res
	return authz.ErrForbidden
}

func TestUsersListAuthorizesAtTheGlobalScope(t *testing.T) {
	c := &captureAuthz{}
	d := Deps{Authz: c, Users: &user.Store{}}
	h := usersHandlerWith(t, d)
	w := scopeReq(t, h, "alice", http.MethodGet, "/users", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if c.got.Owner == nil || c.got.Owner.Kind != meta.OwnerSystem {
		t.Fatalf("authorized against owner %+v, want {system}", c.got.Owner)
	}
}

func usersHandlerWith(t *testing.T, d Deps) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if a, ok := scopeActors[req.Header.Get("X-Test-Actor")]; ok {
				req = req.WithContext(actor.WithActor(req.Context(), a))
			}
			next.ServeHTTP(w, req)
		})
	})
	api := humachi.New(r, huma.DefaultConfig("users-test", "0"))
	registerUsers(api, d, nil)
	return r
}
