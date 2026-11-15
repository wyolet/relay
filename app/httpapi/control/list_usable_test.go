package control

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/meta"
)

func newUsablePoolsHarness(t *testing.T) http.Handler {
	t.Helper()
	return newUsablePoolsHarnessWith(t, authz.AlwaysAllowAuthenticated{})
}

// ownUserRowsAuthz lets a caller write a user-owned row only when it is that user or an admin, as RBAC does; everything else is allowed.
type ownUserRowsAuthz struct{ authz.AlwaysAllowAuthenticated }

func (ownUserRowsAuthz) Authorize(ctx context.Context, action string, r authz.Resource) error {
	if a := actor.From(ctx); r.Owner != nil && r.Owner.Kind == meta.OwnerUser && !authz.IsAdmin(ctx) && (a == nil || a.UserID != r.Owner.ID) {
		return authz.ErrForbidden
	}
	return authz.AlwaysAllowAuthenticated{}.Authorize(ctx, action, r)
}

func newUsablePoolsHarnessWith(t *testing.T, az authz.Authorizer) http.Handler {
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
		"pool-bob":   {Kind: meta.OwnerUser, ID: "u-bob"},
	} {
		store.items[id] = &pool.Pool{Meta: meta.Metadata{ID: id, Name: id, Owner: o}, Spec: pool.Spec{HostID: "h-1"}}
	}
	projects := map[string]*project.Project{
		"p-1": {Meta: meta.Metadata{ID: "p-1", Name: "p-1"}, Spec: project.Spec{TeamID: "t-1"}},
		"p-2": {Meta: meta.Metadata{ID: "p-2", Name: "p-2"}, Spec: project.Spec{TeamID: "t-2"}},
	}
	teams := map[string]*team.Team{"t-1": {Meta: meta.Metadata{ID: "t-1", Name: "t-1"}}, "t-2": {Meta: meta.Metadata{ID: "t-2", Name: "t-2"}}}
	c := refcheck.Checker{Authz: az, Rows: refcheck.Lookup{
		Project: func(_ context.Context, id string) *project.Project { return projects[id] },
		Team:    func(_ context.Context, id string) *team.Team { return teams[id] },
		Pool:    getOrNil(store.Get),
	}}
	r, api := testAPI(t, scopeActors)
	registerKind[pool.Pool](
		api, "pools", "pool", store, authz.AlwaysAllowAuthenticated{}, plmeta, nil, meta.OwnerUser,
		listScanResolver[pool.Pool](store, plmeta),
		nil, nil, nil, nil, nil, noSettings{}, false, nil, &poolFilter,
		usableFor(c, "policy", refcheck.Checker.UsablePool),
	)
	return r
}

// listed asks GET path as who and returns the status, the ids of the rows in the order listed, and total.
func listed(t *testing.T, h http.Handler, who, path string) (int, []string, int) {
	t.Helper()
	w := scopeReq(t, h, who, http.MethodGet, path, "")
	var body struct {
		Items []struct {
			Metadata meta.Metadata `json:"metadata"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: decode %s: %v", path, w.Body, err)
		}
	}
	var names []string
	for _, it := range body.Items {
		names = append(names, it.Metadata.ID)
	}
	return w.Code, names, body.Total
}

// usable_by keeps the rows a policy of that scope may link and composes with the other filters. It takes any scope the caller may write the linking row in: asking for a user's policy leaves out every other user's pools, for an admin too, since no binding of that policy is ever served by them.
func TestListUsableBy(t *testing.T) {
	all := []string{"pool-alice", "pool-bob", "pool-p1", "pool-p2", "pool-sys", "pool-t1", "pool-t2"}
	shared := []string{"pool-p1", "pool-p2", "pool-sys", "pool-t1", "pool-t2"}
	alices := []string{"pool-alice", "pool-p1", "pool-p2", "pool-sys", "pool-t1", "pool-t2"}
	open, ownRows := newUsablePoolsHarness(t), newUsablePoolsHarnessWith(t, ownUserRowsAuthz{})
	for _, tc := range []struct {
		h          http.Handler
		who, query string
		status     int
		want       []string
	}{
		{open, "alice", "", 200, all},
		{open, "alice", "?usable_by=project:p-1", 200, []string{"pool-p1", "pool-sys", "pool-t1"}},
		{open, "alice", "?usable_by=team:t-1", 200, []string{"pool-sys", "pool-t1"}},
		{open, "alice", "?usable_by=user:u-alice", 200, alices},
		{open, "alice", "?usable_by=system", 200, all},
		{open, "alice", "?usable_by=project:p-1&name=pool-t1", 200, []string{"pool-t1"}},
		{open, "alice", "?usable_by=project:p-2&sort=-name", 200, []string{"pool-t2", "pool-sys", "pool-p2"}},
		{open, "alice", "?usable_by=project:p-2&limit=1", 200, []string{"pool-p2"}},
		{open, "root", "?usable_by=user:u-root", 200, shared},
		{open, "token", "?usable_by=user:u-bob", 200, []string{"pool-bob", "pool-p1", "pool-p2", "pool-sys", "pool-t1", "pool-t2"}},
		{ownRows, "root", "?usable_by=user:u-alice", 200, alices},
		{ownRows, "alice", "?usable_by=user:u-alice", 200, alices},
		{ownRows, "bob", "?usable_by=user:u-alice", 403, nil},
		{open, "alice", "?usable_by=project", 400, nil},
		{open, "alice", "?usable_by=project:", 400, nil},
		{open, "alice", "?usable_by=org:o-1", 400, nil},
		{open, "alice", "?usable_by=system&usable_by=team:t-1", 400, nil},
		{open, "alice", "?usable_by=team:a:b", 400, nil},
		{open, "alice", "?usable_by=system&usable_for=pool", 400, nil},
	} {
		code, got, total := listed(t, tc.h, tc.who, "/pools"+tc.query)
		wantTotal := len(tc.want)
		if strings.Contains(tc.query, "limit=1") {
			wantTotal = 3
		}
		if !slices.Equal(got, tc.want) || code != tc.status || total != wantTotal {
			t.Errorf("%s as %s: %d %v (total %d), want %d %v (total %d)", tc.query, tc.who, code, got, total, tc.status, tc.want, wantTotal)
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
	// p-1 sits in t-1, which already reaches acct-t1, so its share comes from t-2.
	tt2 := add(provider("acct-t2", t2), "acct-t2")
	add(account.Share(tt2, account.Receiver{Kind: account.ReceiverProject, ID: "p-1"}, p1, "p-1"), "share-p1")
	projects := map[string]*project.Project{"p-1": {Meta: meta.Metadata{ID: "p-1", Name: "p-1"}, Spec: project.Spec{TeamID: "t-1"}}}
	teams := map[string]*team.Team{"t-1": {Meta: meta.Metadata{ID: "t-1", Name: "t-1"}}, "t-2": {Meta: meta.Metadata{ID: "t-2", Name: "t-2"}}}
	// Another binding already holds acct-p1, so a new binding cannot take it.
	held := []*policybinding.PolicyBinding{{Meta: meta.Metadata{ID: "pb-held", Name: "held", Owner: p1}, Spec: policybinding.Spec{AccountIDs: []string{"acct-p1"}}}}
	bindingReads := 0
	c := refcheck.Checker{Authz: authz.AlwaysAllowAuthenticated{}, Rows: refcheck.Lookup{
		Project:        func(_ context.Context, id string) *project.Project { return projects[id] },
		Team:           func(_ context.Context, id string) *team.Team { return teams[id] },
		Account:        getOrNil(store.Get),
		PolicyBindings: func(context.Context) ([]*policybinding.PolicyBinding, error) { bindingReads++; return held, nil },
	}}
	r, api := testAPI(t, scopeActors)
	registerKind[account.Account](
		api, "accounts", "account", store, authz.AlwaysAllowAuthenticated{}, ameta, nil, meta.OwnerUser,
		listScanResolver[account.Account](store, ameta),
		nil, nil, nil, nil, nil, noSettings{}, false, nil, &accountFilter,
		&usableFilter[account.Account]{checker: c, purposes: []usablePurpose[account.Account]{
			{name: "binding", linker: "a policy binding", check: refcheck.Checker.UsableAccount},
			{name: "pool", linker: "a pool", check: refcheck.Checker.UsablePoolAccount},
		}},
	)
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"?usable_by=team:t-2", []string{"acct-t2", "share-t2"}},
		{"?usable_by=team:t-2&usable_for=binding", []string{"acct-t2", "share-t2"}},
		{"?usable_by=team:t-2&usable_for=pool", []string{"acct-sys", "acct-t2", "share-t2"}},
		{"?usable_by=project:p-1", []string{"share-p1"}},
		{"?usable_by=project:p-1&usable_for=pool", []string{"acct-p1", "acct-sys", "acct-t1", "share-p1"}},
	} {
		bindingReads = 0
		code, got, total := listed(t, r, "alice", "/accounts"+tc.query)
		slices.Sort(got)
		if code != http.StatusOK || !slices.Equal(got, tc.want) || total != len(tc.want) {
			t.Errorf("%s: %d %v (total %d), want %v", tc.query, code, got, total, tc.want)
		}
		// Seven accounts, one read of the bindings: the request's checks share it.
		if bindingReads > 1 {
			t.Errorf("%s: read the policy bindings %d times in one request, want at most 1", tc.query, bindingReads)
		}
	}
	// A scope that does not exist answers as a save naming it would; a user scope is the authorizer's to allow (TestListUsableBy).
	for _, bad := range []string{"?usable_by=team:t-9", "?usable_by=project:p-9&usable_for=pool"} {
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
