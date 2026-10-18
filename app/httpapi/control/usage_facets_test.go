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

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/usage"
)

func newFacetsHarness(t *testing.T, reader *fakeUsageReader, cat *appcatalog.Catalog) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-Actor", scopeActors))
	api := humachi.New(r, huma.DefaultConfig("usage-facets-test", "0"))
	registerUsageFacets(api, Deps{Authz: testRBAC(), UsageReader: reader, Catalog: cat}, nil)
	return r
}

func getFacets(t *testing.T, h http.Handler, who, query string) (int, usage.FacetsResult, string) {
	t.Helper()
	w := scopeReq(t, h, who, http.MethodGet, "/usage/facets?"+query, "")
	var out usage.FacetsResult
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code, out, w.Body.String()
}

func TestUsageFacetsRejectsInvalidDims(t *testing.T) {
	h := newFacetsHarness(t, &fakeUsageReader{}, nil)
	many := make([]string, usage.MaxFacetDims+1)
	for i := range many {
		many[i] = "tags.k" + string(rune('a'+i))
	}
	for _, q := range []string{
		"",
		"dims=",
		"dims=none",
		"dims=bogus",
		"dims=model_id,,host_id",
		"dims=model_id,model_id",
		"dims=tags.",
		"dims=" + strings.Join(many, ","),
		"dims=model_id&limit=201",
		"dims=model_id&limit=-1",
	} {
		if code, _, body := getFacets(t, h, "root", q); code != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want 400: %s", q, code, body)
		}
	}
	if code, _, body := getFacets(t, h, "root", "dims=model_id,tags.env,extras.instance&limit=200"); code != http.StatusOK {
		t.Errorf("valid request: status = %d: %s", code, body)
	}
}

func TestUsageFacetsScopesToReadableRows(t *testing.T) {
	reader := &fakeUsageReader{events: []usagelog.Event{
		{RequestID: "r-1", Timestamp: time.Now(), Status: 200, PrincipalID: "u-alice", ModelID: "m-own", Model: "own"},
		{RequestID: "r-2", Timestamp: time.Now(), Status: 200, PrincipalID: "u-bob", ModelID: "m-foreign", Model: "foreign"},
	}}
	h := newFacetsHarness(t, reader, nil)

	code, out, body := getFacets(t, h, "alice", "dims=model_id")
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, body)
	}
	want := map[string][]usage.FacetValue{"model_id": {{Value: "m-own", Label: "own", Requests: 1}}}
	if !reflect.DeepEqual(out.Facets, want) {
		t.Fatalf("facets = %+v, want %+v", out.Facets, want)
	}

	reader.calls = 0
	code, out, body = getFacets(t, h, "nobody", "dims=model_id,host_id")
	if code != http.StatusOK {
		t.Fatalf("scoped-out status = %d: %s", code, body)
	}
	if reader.calls != 0 {
		t.Fatalf("reader called %d times for a caller who may read nothing", reader.calls)
	}
	if len(out.Facets) != 2 || len(out.Facets["model_id"]) != 0 || out.Facets["host_id"] == nil {
		t.Fatalf("facets = %+v, want both dims present and empty", out.Facets)
	}
}

// Rows written before an id had a slug beside it still get a name, from the catalog as it is now.
func TestUsageFacetsFallsBackToCatalogLabels(t *testing.T) {
	yes := true
	k := &key.Key{Meta: meta.Metadata{ID: ids.New(), Name: "ci-key", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	k.Spec.Enabled = &yes
	k.Spec.KeyHash = "hash-ci"
	tm := &team.Team{Meta: meta.Metadata{ID: ids.New(), Name: "platform", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	tm.Spec.Enabled = &yes
	cat := catalogtest.Catalog{Keys: []*key.Key{k}, Teams: []*team.Team{tm}}.Load(t)

	reader := &fakeUsageReader{events: []usagelog.Event{
		{RequestID: "r-1", Timestamp: time.Now(), Status: 200, RelayKeyHash: "hash-ci", TeamID: tm.Meta.ID},
		{RequestID: "r-2", Timestamp: time.Now(), Status: 200, RelayKeyHash: "hash-gone", RelayKey: "stamped", TeamID: "t-gone"},
		{RequestID: "r-3", Timestamp: time.Now(), Status: 200, RelayKeyHash: "hash-gone", TeamID: "t-gone"},
	}}
	h := newFacetsHarness(t, reader, cat)
	code, out, body := getFacets(t, h, "root", "dims=relay_key_hash,team_id,relay_key")
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, body)
	}
	want := map[string][]usage.FacetValue{
		"relay_key_hash": {{Value: "hash-gone", Label: "stamped", Requests: 2}, {Value: "hash-ci", Label: "ci-key", Requests: 1}},
		"team_id":        {{Value: "t-gone", Requests: 2}, {Value: tm.Meta.ID, Label: "platform", Requests: 1}},
		"relay_key":      {{Value: "", Requests: 2}, {Value: "stamped", Requests: 1}},
	}
	if !reflect.DeepEqual(out.Facets, want) {
		t.Fatalf("facets = %+v\nwant %+v", out.Facets, want)
	}
	if strings.Contains(body, `"label":""`) {
		t.Fatalf("an empty label must be omitted: %s", body)
	}
}
