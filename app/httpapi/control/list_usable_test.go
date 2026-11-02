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

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/team"
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
	teams := map[string]*team.Team{"t-1": {Meta: meta.Metadata{ID: "t-1", Name: "t-1"}}, "t-2": {Meta: meta.Metadata{ID: "t-2", Name: "t-2"}}}
	c := refcheck.Checker{Authz: authz.AlwaysAllowAuthenticated{}, Rows: refcheck.Lookup{
		Project: func(_ context.Context, id string) *project.Project { return projects[id] },
		Team:    func(_ context.Context, id string) *team.Team { return teams[id] },
		Pool:    getOrNil(store.Get),
	}}
	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-Actor", scopeActors))
	api := humachi.New(r, huma.DefaultConfig("usable-test", "0"))
	registerKind[pool.Pool](
		api, "pools", "pool", store, authz.AlwaysAllowAuthenticated{}, plmeta, nil, meta.OwnerUser,
		listScanResolver[pool.Pool](store, plmeta),
		nil, nil, nil, nil, nil, noSettings{}, false, nil, &poolFilter,
		usableFor(c, "a policy", c.UsablePool),
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
	param := func(path, name string) map[string]any {
		params, _ := operation(t, doc, path, "get")["parameters"].([]any)
		for _, p := range params {
			if m, _ := p.(map[string]any); m["name"] == name {
				return m
			}
		}
		return nil
	}
	has := func(path string) bool { return param(path, usableByParam) != nil }
	for _, path := range []string{"/pools", "/policies", "/host-keys"} {
		if param(path, usableForParam) != nil {
			t.Errorf("GET %s offers %s", path, usableForParam)
		}
	}
	if p := param("/accounts", usableForParam); p == nil {
		t.Errorf("GET /accounts lacks %s", usableForParam)
	} else if s, _ := p["schema"].(map[string]any); s["default"] != "binding" || len(s["enum"].([]any)) != 2 {
		t.Errorf("GET /accounts %s schema = %v, want enum binding|pool, default binding", usableForParam, s)
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

// On accounts usable_for picks the linking row: a policy binding (the default) binds accounts inside its scope; a pool lists the provider accounts its scope reaches and the shares made with that scope.
func TestListAccountsUsableFor(t *testing.T) {
	ameta := func(a *account.Account) *meta.Metadata { return &a.Meta }
	store := &memStore[account.Account]{metaOf: ameta, items: map[string]*account.Account{}}
	add := func(a *account.Account, id string) *account.Account {
		a.Meta.ID = id
		store.items[id] = a
		return a
	}
	provider := func(name string, o meta.Owner) *account.Account {
		return &account.Account{Meta: meta.Metadata{Name: name, Owner: o}, Spec: account.Spec{HostID: "h-1"}}
	}
	t2, p1 := meta.Owner{Kind: meta.OwnerTeam, ID: "t-2"}, meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}
	add(provider("acct-sys", meta.Owner{Kind: meta.OwnerSystem}), "acct-sys")
	t1 := add(provider("acct-t1", meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}), "acct-t1")
	add(provider("acct-p1", p1), "acct-p1")
	add(provider("acct-alice", meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}), "acct-alice")
	add(account.Share(t1, account.Receiver{Kind: account.ReceiverTeam, ID: "t-2"}, t2, "t-2"), "share-t2")
	add(account.Share(t1, account.Receiver{Kind: account.ReceiverProject, ID: "p-1"}, p1, "p-1"), "share-p1")
	projects := map[string]*project.Project{"p-1": {Meta: meta.Metadata{ID: "p-1", Name: "p-1"}, Spec: project.Spec{TeamID: "t-1"}}}
	teams := map[string]*team.Team{"t-1": {Meta: meta.Metadata{ID: "t-1", Name: "t-1"}}, "t-2": {Meta: meta.Metadata{ID: "t-2", Name: "t-2"}}}
	// Another binding already holds acct-p1, so a new binding cannot take it.
	held := []*policybinding.PolicyBinding{{Meta: meta.Metadata{ID: "pb-held", Name: "held", Owner: p1}, Spec: policybinding.Spec{AccountIDs: []string{"acct-p1"}}}}
	c := refcheck.Checker{Authz: authz.AlwaysAllowAuthenticated{}, Rows: refcheck.Lookup{
		Project:        func(_ context.Context, id string) *project.Project { return projects[id] },
		Team:           func(_ context.Context, id string) *team.Team { return teams[id] },
		Account:        getOrNil(store.Get),
		PolicyBindings: func(context.Context) ([]*policybinding.PolicyBinding, error) { return held, nil },
	}}
	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-Actor", scopeActors))
	api := humachi.New(r, huma.DefaultConfig("usable-for-test", "0"))
	registerKind[account.Account](
		api, "accounts", "account", store, authz.AlwaysAllowAuthenticated{}, ameta, nil, meta.OwnerUser,
		listScanResolver[account.Account](store, ameta),
		nil, nil, nil, nil, nil, noSettings{}, false, nil, &accountFilter,
		&usableFilter[account.Account]{scope: c.UsableScope, purposes: []usablePurpose[account.Account]{
			{name: "binding", linker: "a policy binding", check: c.UsableAccount},
			{name: "pool", linker: "a pool", check: c.UsablePoolAccount},
		}},
	)
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"?usable_by=team:t-2", []string{"share-t2"}},
		{"?usable_by=team:t-2&usable_for=binding", []string{"share-t2"}},
		{"?usable_by=team:t-2&usable_for=pool", []string{"acct-sys", "share-t2"}},
		{"?usable_by=project:p-1", []string{"share-p1"}},
		{"?usable_by=project:p-1&usable_for=pool", []string{"acct-p1", "acct-sys", "acct-t1", "share-p1"}},
	} {
		w := scopeReq(t, r, "alice", http.MethodGet, "/accounts"+tc.query, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", tc.query, w.Code, w.Body)
		}
		var body struct {
			Items []account.Account `json:"items"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, a := range body.Items {
			got = append(got, a.Meta.ID)
		}
		slices.Sort(got)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.query, got, tc.want)
		}
	}
	// A scope a save could not own a row in answers as that save would.
	for _, bad := range []string{"?usable_by=team:t-9", "?usable_by=project:p-9&usable_for=pool", "?usable_by=user:u-bob"} {
		if w := scopeReq(t, r, "alice", http.MethodGet, "/accounts"+bad, ""); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400: %s", bad, w.Code, w.Body)
		}
	}
	for _, bad := range []string{"?usable_for=pool", "?usable_by=system&usable_for=key", "?usable_by=system&usable_for=pool&usable_for=binding"} {
		if w := scopeReq(t, r, "alice", http.MethodGet, "/accounts"+bad, ""); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400: %s", bad, w.Code, w.Body)
		}
	}
	if w := scopeReq(t, newUsablePoolsHarness(t), "alice", http.MethodGet, "/pools?usable_by=system&usable_for=pool", ""); w.Code != http.StatusBadRequest {
		t.Errorf("usable_for on pools: status %d, want 400", w.Code)
	}
}
