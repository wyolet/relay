//go:build integration

package control

import (
	"reflect"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/usage"
)

// Events never carry a user principal's slug, so the facet names users from the users table.
func TestUsageFacetsNameUserPrincipals(t *testing.T) {
	users := user.NewStore(storagetest.Pool(t))
	alice := &user.User{ID: ids.New(), Username: "alice"}
	if err := users.Upsert(t.Context(), alice); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	gone := ids.New()
	reader := &fakeUsageReader{events: []usagelog.Event{
		{RequestID: "r-1", Timestamp: time.Now(), Status: 200, PrincipalKind: "user", PrincipalID: alice.ID},
		{RequestID: "r-2", Timestamp: time.Now(), Status: 200, PrincipalKind: "user", PrincipalID: alice.ID},
		{RequestID: "r-3", Timestamp: time.Now(), Status: 200, PrincipalKind: "user", PrincipalID: gone},
	}}
	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-Actor", scopeActors))
	api := humachi.New(r, huma.DefaultConfig("usage-facets-users-test", "0"))
	registerUsageFacets(api, Deps{Authz: testRBAC(), UsageReader: reader, Users: users}, nil)

	code, out, body := getFacets(t, r, "root", "dims=principal_id")
	if code != 200 {
		t.Fatalf("status = %d: %s", code, body)
	}
	want := map[string][]usage.FacetValue{"principal_id": {
		{Value: alice.ID, Label: "alice", Requests: 2},
		{Value: gone, Requests: 1},
	}}
	if !reflect.DeepEqual(out.Facets, want) {
		t.Fatalf("facets = %+v, want %+v", out.Facets, want)
	}
}
