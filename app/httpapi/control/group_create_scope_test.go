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
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/meta"
)

// newGroupsHarness mounts registerKind for the real group kind — Team,
// Group and Role default to meta.OwnerSystem on create (crud.go's
// registerKind wiring), so a plain user's create can no longer fall back
// to the personal-row rule the way it does for a user-owned kind.
func newGroupsHarness(t *testing.T, authzr authz.Authorizer) http.Handler {
	t.Helper()
	gmeta := func(g *group.Group) *meta.Metadata { return &g.Meta }
	store := &memStore[group.Group]{metaOf: gmeta, items: map[string]*group.Group{}}

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if a, ok := scopeActors[req.Header.Get("X-Test-Actor")]; ok {
				req = req.WithContext(actor.WithActor(req.Context(), a))
			}
			next.ServeHTTP(w, req)
		})
	})
	api := humachi.New(r, huma.DefaultConfig("groups-scope-test", "0"))
	registerKind[group.Group](
		api, "groups", "group", store, authzr, gmeta,
		func(g *group.Group) error { return g.Validate() },
		meta.OwnerSystem,
		listScanResolver[group.Group](store, gmeta),
		nil, nil, nil, nil, nil,
		noSettings{},
		false,
		nil,
		nil,
	)
	return r
}

func TestGroupCreateNeedsAnAdminNotAPersonalRow(t *testing.T) {
	h := newGroupsHarness(t, testRBAC())

	// A plain authenticated user with no bindings gets denied: the row
	// defaults to owner.kind=system, so the personal-row rule never fires.
	w := scopeReq(t, h, "alice", http.MethodPost, "/groups", `{"metadata":{"name":"eng","displayName":"Eng"},"spec":{}}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin create = %d, want 403: %s", w.Code, w.Body)
	}

	// The admin actor is unconditionally allowed, and the row lands system-owned.
	w = scopeReq(t, h, "root", http.MethodPost, "/groups", `{"metadata":{"name":"eng","displayName":"Eng"},"spec":{}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("admin create = %d, want 201: %s", w.Code, w.Body)
	}
	var created group.Group
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Meta.Owner.Kind != meta.OwnerSystem {
		t.Fatalf("created owner.kind = %q, want %q", created.Meta.Owner.Kind, meta.OwnerSystem)
	}
}
