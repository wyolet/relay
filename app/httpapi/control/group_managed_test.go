package control

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/auth/directory"
)

// managedGroups refuses every write the way the directory refuses one to a group a provisioning source manages.
type managedGroups struct{ *memStore[group.Group] }

func (s managedGroups) Upsert(context.Context, *group.Group) error {
	return meta.FromDirectoryError(&directory.ManagedError{Kind: "group", Name: "ops", Source: "scim-okta"})
}

func (s managedGroups) Delete(context.Context, string) error {
	return &directory.ManagedError{Kind: "group", Name: "ops", Source: "scim-okta"}
}

// A UI edit or delete of a managed group is the caller's conflict, named after the source, not a server failure.
func TestManagedGroupWriteIsAConflict(t *testing.T) {
	gmeta := func(g *group.Group) *meta.Metadata { return &g.Meta }
	id := "01a12c00-0000-7000-8000-000000000001"
	store := managedGroups{&memStore[group.Group]{metaOf: gmeta, items: map[string]*group.Group{
		id: {Meta: meta.Metadata{ID: id, Name: "ops", DisplayName: "Ops", Owner: meta.Owner{Kind: meta.OwnerSystem}}},
	}}}
	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-Actor", scopeActors))
	api := humachi.New(r, huma.DefaultConfig("groups-managed-test", "0"))
	registerKind[group.Group](api, "groups", "group", store, testRBAC(), gmeta,
		func(g *group.Group) error { return g.Validate() }, meta.OwnerSystem,
		listScanResolver[group.Group](store, gmeta), nil, nil, nil, nil, nil, noSettings{}, false, nil, nil)

	body := `{"metadata":{"id":"` + id + `","name":"ops","displayName":"Ops 2","owner":{"kind":"system"}},"spec":{}}`
	for method, req := range map[string]string{http.MethodPut: body, http.MethodDelete: ""} {
		w := scopeReq(t, r, "root", method, "/groups/by-id/"+id, req)
		if w.Code != http.StatusConflict {
			t.Fatalf("%s of a managed group = %d, want 409: %s", method, w.Code, w.Body)
		}
		var got struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: decode %s: %v", method, w.Body, err)
		}
		if got.Error.Code != "managed_by_source" || got.Error.Message != "group is managed by scim-okta; apply with force to override" {
			t.Fatalf("%s: error = %+v", method, got.Error)
		}
	}
}
