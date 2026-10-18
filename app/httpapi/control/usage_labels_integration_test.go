//go:build integration

package control

import (
	"encoding/json"
	"net/http"
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
)

// Grouping by principal_id names user principals from the users table on both summary and timeseries.
func TestUsageGroupLabelsNameUserPrincipals(t *testing.T) {
	users := user.NewStore(storagetest.Pool(t))
	alice := &user.User{ID: ids.New(), Username: "alice"}
	if err := users.Upsert(t.Context(), alice); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	reader := &fakeUsageReader{events: []usagelog.Event{
		{RequestID: "r-1", Timestamp: time.Now(), Status: 200, PrincipalKind: "user", PrincipalID: alice.ID},
		{RequestID: "r-2", Timestamp: time.Now(), Status: 200, PrincipalKind: "user", PrincipalID: ids.New()},
	}}
	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-Actor", scopeActors))
	api := humachi.New(r, huma.DefaultConfig("usage-labels-users-test", "0"))
	registerUsage(api, Deps{Authz: testRBAC(), UsageReader: reader, Users: users}, nil)

	for _, path := range []string{"/usage/summary?", "/usage/timeseries?interval=1h&"} {
		w := scopeReq(t, r, "root", http.MethodGet, path+"group_by=principal_id", "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", path, w.Code, w.Body)
		}
		var out struct {
			Labels map[string]string `json:"labels"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if want := map[string]string{alice.ID: "alice"}; !reflect.DeepEqual(out.Labels, want) {
			t.Errorf("%s: labels = %v, want %v", path, out.Labels, want)
		}
	}
}
