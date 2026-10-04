package control

import (
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/rolebinding"
)

// newRoleBindingsHarness mounts the real role-binding kind, whose owner
// mirrors spec.scope rather than defaulting to one kind.
func newRoleBindingsHarness(t *testing.T) http.Handler {
	t.Helper()
	rbmeta := func(b *rolebinding.RoleBinding) *meta.Metadata { return &b.Meta }
	store := &memStore[rolebinding.RoleBinding]{metaOf: rbmeta, items: map[string]*rolebinding.RoleBinding{}}

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if a, ok := scopeActors[req.Header.Get("X-Test-Actor")]; ok {
				req = req.WithContext(actor.WithActor(req.Context(), a))
			}
			next.ServeHTTP(w, req)
		})
	})
	api := humachi.New(r, huma.DefaultConfig("rolebindings-test", "0"))
	registerKind[rolebinding.RoleBinding](
		api, "role-bindings", "role-binding", store, authz.AlwaysAllowAuthenticated{}, rbmeta,
		func(b *rolebinding.RoleBinding) error { b.StampOwner(); return b.Validate() },
		"",
		listScanResolver[rolebinding.RoleBinding](store, rbmeta),
		nil, nil, nil, nil, nil,
		noSettings{},
		false,
		nil,
		nil,
	)
	return r
}

// The global scope is spelled {kind: system}, and a role binding's owner
// mirrors its scope — so the reserved-owner guard must not reject the one
// shape a global binding can have.
func TestCreateGlobalRoleBindingIsNotReservedOwner(t *testing.T) {
	h := newRoleBindingsHarness(t)
	body := `{"metadata":{"name":"ops-admin","displayName":"Ops Admin","owner":{"kind":"system"}},` +
		`"spec":{"roleId":"00000000-0000-7000-8000-000000000001",` +
		`"scope":{"kind":"system"},"subjects":[{"kind":"user","id":"00000000-0000-7000-8000-000000000002"}]}}`
	w := scopeReq(t, h, "root", http.MethodPost, "/role-bindings", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body)
	}
}
