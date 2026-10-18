package control

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/ids"
)

// Group values whose events carry no slug (rows older than the slug) are named from the catalog as it is now; a stamped slug wins over the catalog.
func TestUsageGroupLabelsFallBackToCatalog(t *testing.T) {
	yes := true
	k := &key.Key{Meta: meta.Metadata{ID: ids.New(), Name: "ci-key", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	k.Spec.Enabled = &yes
	k.Spec.KeyHash = "hash-ci"
	tm := &team.Team{Meta: meta.Metadata{ID: ids.New(), Name: "platform", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	tm.Spec.Enabled = &yes
	cat := catalogtest.Catalog{Keys: []*key.Key{k}, Teams: []*team.Team{tm}}.Load(t)

	now := time.Now()
	reader := &fakeUsageReader{events: []usagelog.Event{
		{RequestID: "r-1", Timestamp: now, Status: 200, RelayKeyHash: "hash-ci", TeamID: tm.Meta.ID},
		{RequestID: "r-2", Timestamp: now, Status: 200, RelayKeyHash: "hash-gone", RelayKey: "stamped", TeamID: "t-gone"},
		{RequestID: "r-3", Timestamp: now, Status: 200, RelayKeyHash: k.Spec.KeyHash, RelayKey: "ci-key-renamed"},
	}}
	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-Actor", scopeActors))
	api := humachi.New(r, huma.DefaultConfig("usage-labels-test", "0"))
	registerUsage(api, Deps{Authz: testRBAC(), UsageReader: reader, Catalog: cat}, nil)

	for _, tc := range []struct {
		groupBy string
		want    map[string]string
	}{
		{"relay_key_hash", map[string]string{"hash-ci": "ci-key-renamed", "hash-gone": "stamped"}},
		{"team_id", map[string]string{tm.Meta.ID: "platform"}},
		{"relay_key", nil},
	} {
		for _, path := range []string{"/usage/summary?", "/usage/timeseries?interval=1h&"} {
			w := scopeReq(t, r, "root", http.MethodGet, path+"group_by="+tc.groupBy, "")
			if w.Code != http.StatusOK {
				t.Fatalf("%s group_by=%s: status = %d: %s", path, tc.groupBy, w.Code, w.Body)
			}
			var out struct {
				Labels map[string]string `json:"labels"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(out.Labels, tc.want) {
				t.Errorf("%s group_by=%s: labels = %v, want %v", path, tc.groupBy, out.Labels, tc.want)
			}
			if tc.want == nil && strings.Contains(w.Body.String(), `"labels"`) {
				t.Errorf("%s group_by=%s: labels must be omitted: %s", path, tc.groupBy, w.Body)
			}
		}
	}
}

// Principal groups keep a store slug, then take a service account's catalog name, and only what is still missing goes to the users table, in one lookup.
func TestGroupLabelsNameUserPrincipalsInOneLookup(t *testing.T) {
	yes := true
	tm := &team.Team{Meta: meta.Metadata{ID: ids.New(), Name: "t", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	tm.Spec.Enabled = &yes
	proj := &project.Project{Meta: meta.Metadata{ID: ids.New(), Name: "p", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: tm.Meta.ID}}}
	proj.Spec.TeamID, proj.Spec.Enabled = tm.Meta.ID, &yes
	sa := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: ids.New(), Name: "deployer", Owner: meta.Owner{Kind: meta.OwnerProject, ID: proj.Meta.ID}}}
	sa.Spec.ProjectID, sa.Spec.Enabled = proj.Meta.ID, &yes
	cat := catalogtest.Catalog{Teams: []*team.Team{tm}, Projects: []*project.Project{proj}, ServiceAccounts: []*serviceaccount.ServiceAccount{sa}}.Load(t)

	groups := []map[string]string{
		{"principal_id": "u-alice"}, {"principal_id": "sa-slugged"}, {"principal_id": sa.Meta.ID}, {"principal_id": "u-gone"}, {"principal_id": ""},
	}
	users := &fakeUsernames{names: map[string]string{"u-alice": "alice", "sa-slugged": "not-used", sa.Meta.ID: "not-used"}}
	got := fillGroupLabels(t.Context(), cat, users, "principal_id", groups, map[string]string{"sa-slugged": "ci-bot"})
	if want := [][]string{{"u-alice", "u-gone"}}; !reflect.DeepEqual(users.calls, want) {
		t.Fatalf("lookups = %v, want %v", users.calls, want)
	}
	if want := map[string]string{"u-alice": "alice", "sa-slugged": "ci-bot", sa.Meta.ID: "deployer"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}

	other := &fakeUsernames{}
	fillGroupLabels(t.Context(), cat, other, "team_id", []map[string]string{{"team_id": "t-gone"}}, nil)
	if len(other.calls) != 0 {
		t.Fatalf("a non-principal grouping queried users: %v", other.calls)
	}
	if got := fillGroupLabels(t.Context(), nil, nil, "principal_id", groups, nil); got != nil {
		t.Fatalf("no catalog and no users store: labels = %v, want nil", got)
	}
}
