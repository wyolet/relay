package control

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/refcheck"
)

func newUsablePoolsHarness(t *testing.T) http.Handler {
	t.Helper()
	plmeta := func(p *pool.Pool) *meta.Metadata { return &p.Meta }
	store := &memStore[pool.Pool]{metaOf: plmeta, items: map[string]*pool.Pool{}}
	for id, o := range map[string]meta.Owner{
		"pool-sys":   {Kind: meta.OwnerSystem},
		"pool-t1":    {Kind: meta.OwnerTeam, ID: "t-1"},
		"pool-t2":    {Kind: meta.OwnerTeam, ID: "t-2"},
		"pool-p1":    {Kind: meta.OwnerProject, ID: "p-1"},
		"pool-p2":    {Kind: meta.OwnerProject, ID: "p-2"},
		"pool-alice": {Kind: meta.OwnerUser, ID: "u-alice"},
	} {
		store.items[id] = &pool.Pool{Meta: meta.Metadata{ID: id, Name: id, Owner: o}, Spec: pool.Spec{HostID: "h-1"}}
	}
	projects := map[string]*project.Project{
		"p-1": {Meta: meta.Metadata{ID: "p-1", Name: "p-1"}, Spec: project.Spec{TeamID: "t-1"}},
		"p-2": {Meta: meta.Metadata{ID: "p-2", Name: "p-2"}, Spec: project.Spec{TeamID: "t-2"}},
	}
	c := refcheck.Checker{Authz: authz.AlwaysAllowAuthenticated{}, Rows: refcheck.Lookup{
		Project: func(_ context.Context, id string) *project.Project { return projects[id] },
	}}
	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-Actor", scopeActors))
	api := humachi.New(r, huma.DefaultConfig("usable-test", "0"))
	registerKind[pool.Pool](
		api, "pools", "pool", store, authz.AlwaysAllowAuthenticated{}, plmeta, nil, meta.OwnerUser,
		listScanResolver[pool.Pool](store, plmeta),
		nil, nil, nil, nil, nil, noSettings{}, false, nil, &poolFilter,
		&usableFilter[pool.Pool]{linker: "a policy", check: c.UsablePool},
	)
	return r
}

// usable_by keeps the rows a policy of that scope may link, composes with the other filters, and refuses a malformed scope.
func TestListUsableBy(t *testing.T) {
	h := newUsablePoolsHarness(t)
	all := []string{"pool-alice", "pool-p1", "pool-p2", "pool-sys", "pool-t1", "pool-t2"}
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"", all},
		{"?usable_by=project:p-1", []string{"pool-p1", "pool-sys", "pool-t1"}},
		{"?usable_by=team:t-1", []string{"pool-sys", "pool-t1"}},
		{"?usable_by=user:u-alice", all},
		{"?usable_by=system", all},
		{"?usable_by=project:p-1&name=pool-t1", []string{"pool-t1"}},
		{"?usable_by=project:p-2&sort=-name", []string{"pool-t2", "pool-sys", "pool-p2"}},
	} {
		w := scopeReq(t, h, "alice", http.MethodGet, "/pools"+tc.query, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", tc.query, w.Code, w.Body)
		}
		var body struct {
			Items []pool.Pool `json:"items"`
			Total int         `json:"total"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, p := range body.Items {
			got = append(got, p.Meta.Name)
		}
		if !strings.Contains(tc.query, "sort=") {
			slices.Sort(got)
		}
		if !slices.Equal(got, tc.want) || body.Total != len(tc.want) {
			t.Errorf("%s: got %v (total %d), want %v", tc.query, got, body.Total, tc.want)
		}
	}
	for _, bad := range []string{"?usable_by=project", "?usable_by=project:", "?usable_by=org:o-1", "?usable_by=system&usable_by=team:t-1", "?usable_by=team:a:b"} {
		if w := scopeReq(t, h, "alice", http.MethodGet, "/pools"+bad, ""); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400: %s", bad, w.Code, w.Body)
		}
	}
}

// The parameter is in the generated document on the linkable kinds only.
func TestControlOpenAPIHasUsableBy(t *testing.T) {
	doc := controlDoc(t)
	has := func(path string) bool {
		params, _ := operation(t, doc, path, "get")["parameters"].([]any)
		for _, p := range params {
			if m, _ := p.(map[string]any); m["name"] == usableByParam {
				return true
			}
		}
		return false
	}
	for _, path := range []string{"/pools", "/policies", "/rate-limits", "/budgets", "/accounts", "/host-keys"} {
		if !has(path) {
			t.Errorf("GET %s lacks %s", path, usableByParam)
		}
	}
	for _, path := range []string{"/teams", "/projects", "/keys", "/policy-bindings"} {
		if has(path) {
			t.Errorf("GET %s offers %s", path, usableByParam)
		}
	}
}
