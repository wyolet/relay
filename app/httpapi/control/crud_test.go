package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

// scopedThing is a kind with metadata alone, for registerKind's own rules.
type scopedThing struct {
	Meta meta.Metadata `json:"metadata"`
}

func thingMeta(v *scopedThing) *meta.Metadata { return &v.Meta }

type memStore[T any] struct {
	metaOf func(*T) *meta.Metadata
	items  map[string]*T
}

func newMemStore[T any](metaOf func(*T) *meta.Metadata, seed ...*T) *memStore[T] {
	s := &memStore[T]{metaOf: metaOf, items: map[string]*T{}}
	for _, it := range seed {
		s.items[metaOf(it).ID] = it
	}
	return s
}

func (s *memStore[T]) List(context.Context) ([]*T, error) {
	out := make([]*T, 0, len(s.items))
	for _, it := range s.items {
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return s.metaOf(out[i]).ID < s.metaOf(out[j]).ID })
	return out, nil
}

func (s *memStore[T]) Get(_ context.Context, id string) (*T, error) {
	it, ok := s.items[id]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return it, nil
}

func (s *memStore[T]) Upsert(_ context.Context, t *T) error {
	s.items[s.metaOf(t).ID] = t
	return nil
}

func (s *memStore[T]) Delete(_ context.Context, id string) error {
	if _, ok := s.items[id]; !ok {
		return fmt.Errorf("not found")
	}
	delete(s.items, id)
	return nil
}

// govSettings answers governance sections by name; a missing one takes the default.
type govSettings map[string]*settings.Governance

func (g govSettings) Setting(section string) (any, bool) {
	v, ok := g[section]
	return v, ok
}

type noSettings struct{}

func (noSettings) Setting(string) (any, bool) { return nil, false }

// testRBAC is the enforcing authorizer over an empty catalog: every allow it grants comes from the rules that need no binding.
func testRBAC() authz.RBAC {
	snap := catalogtest.Catalog{}.Snapshot()
	return authz.RBAC{Snap: func() authz.Snapshot { return snap }}
}

var scopeActors = map[string]*actor.Actor{
	"alice": {UserID: "u-alice", Username: "alice"},
	"bob":   {UserID: "u-bob", Username: "bob"},
	"root":  {UserID: "u-root", Username: "root", Roles: []string{user.RoleAdmin}},
	"token": {AdminToken: true, Username: "admin-token"},
	// granted holds no admin role on its session; a role binding may make it one.
	"granted": {UserID: "u-granted", Username: "granted", Subjects: appcatalog.UserSubjects("u-granted", nil, nil)},
}

// testAPI is a router whose requests run as the actor X-Test-Actor names, with the relay error envelope installed.
func testAPI(t *testing.T, actors map[string]*actor.Actor, mw ...func(http.Handler) http.Handler) (*chi.Mux, huma.API) {
	t.Helper()
	httpapi.Install()
	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-Actor", actors))
	r.Use(mw...)
	cfg := huma.DefaultConfig(t.Name(), "0")
	cfg.OpenAPI.Components.Schemas = httpapi.NewRegistry()
	return r, humachi.New(r, cfg)
}

// mountKind registers one kind over store with none of the optional hooks.
func mountKind[T any](api huma.API, plural, singular string, store entityStore[T], authzr authz.Authorizer,
	metaOf func(*T) *meta.Metadata, validate func(*T) error, defaultOwner meta.OwnerKind, guard mutationGuard[T], gov settings.Reader,
) kindRows {
	return registerKind[T](api, plural, singular, store, authzr, metaOf, validate, defaultOwner,
		listScanResolver[T](store, metaOf), guard, nil, nil, nil, nil, gov, false, nil, nil, nil)
}

// newThingHarness serves scopedThing rows as plural under authzr and gov.
func newThingHarness(t *testing.T, authzr authz.Authorizer, plural, singular string, gov settings.Reader, seed ...*scopedThing) (http.Handler, *memStore[scopedThing]) {
	t.Helper()
	store := newMemStore(thingMeta, seed...)
	r, api := testAPI(t, scopeActors)
	mountKind[scopedThing](api, plural, singular, store, authzr, thingMeta, nil, meta.OwnerUser, nil, gov)
	return r, store
}

func newScopeHarness(t *testing.T, authzr authz.Authorizer, seed ...*scopedThing) (http.Handler, *memStore[scopedThing]) {
	t.Helper()
	return newThingHarness(t, authzr, "rate-limits", "rate-limit", noSettings{}, seed...)
}

func scopeReq(t *testing.T, h http.Handler, who, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if who != "" {
		req.Header.Set("X-Test-Actor", who)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// wantAPIError checks a refusal's status and its error envelope's code and message.
func wantAPIError(t *testing.T, w *httptest.ResponseRecorder, status int, code, message string) {
	t.Helper()
	var got struct {
		Error httpapi.APIErrorBody `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("status %d, body is not the error envelope (%v): %s", w.Code, err, w.Body)
	}
	if w.Code != status || got.Error.Code != code || got.Error.Message != message {
		t.Fatalf("got %d code %q message %q, want %d code %q message %q", w.Code, got.Error.Code, got.Error.Message, status, code, message)
	}
}

// wantFieldError checks a 422 that names exactly one field and what is wrong with it.
func wantFieldError(t *testing.T, w *httptest.ResponseRecorder, location, message string) {
	t.Helper()
	var got struct {
		Error httpapi.APIErrorBody `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 422 || got.Error.Code != "unprocessable_entity" ||
		len(got.Error.Details) != 1 || got.Error.Details[0].Location != location || got.Error.Details[0].Message != message {
		t.Fatalf("got %d %s (%v), want a 422 with one detail at %s: %s", w.Code, w.Body, err, location, message)
	}
}

// decodeMeta returns the metadata of the row a 2xx answer carries.
func decodeMeta(t *testing.T, w *httptest.ResponseRecorder, status int) meta.Metadata {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d: %s", w.Code, status, w.Body)
	}
	var out struct {
		Metadata meta.Metadata `json:"metadata"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", w.Body, err)
	}
	return out.Metadata
}

// wantJSON checks the answer's status and decodes its body into out.
func wantJSON(t *testing.T, w *httptest.ResponseRecorder, status int, out any) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d: %s", w.Code, status, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
		t.Fatalf("decode %s: %v", w.Body, err)
	}
}

func createdID(t *testing.T, w interface{ Bytes() []byte }) string {
	t.Helper()
	var out struct {
		Metadata meta.Metadata `json:"metadata"`
	}
	if err := json.Unmarshal(w.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.Metadata.ID
}

const (
	aliceID   = "01950000-0000-7000-8000-0000000000a1"
	bobID     = "01950000-0000-7000-8000-0000000000b1"
	catalogID = "01950000-0000-7000-8000-0000000000c1"
	projectID = "01950000-0000-7000-8000-0000000000d1"
	systemID  = "01950000-0000-7000-8000-0000000000e1"
	unknownID = "01950000-0000-7000-8000-0000000000f1"
)

// seedThings is one row per owner tier, in id order.
func seedThings() []*scopedThing {
	return []*scopedThing{
		{Meta: meta.Metadata{ID: aliceID, Name: "alice-row", Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}}},
		{Meta: meta.Metadata{ID: bobID, Name: "bob-row", Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-bob"}}},
		{Meta: meta.Metadata{ID: catalogID, Name: "catalog-row", Owner: meta.Owner{Kind: meta.OwnerHost, ID: "h-1"}}},
		{Meta: meta.Metadata{ID: projectID, Name: "project-row", Owner: meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}}},
		{Meta: meta.Metadata{ID: systemID, Name: "system-row", Owner: meta.Owner{Kind: meta.OwnerSystem}}},
	}
}

// A list holds the rows the caller may see, and total counts those alone.
func TestCRUDListIsScopedToTheCaller(t *testing.T) {
	all := []string{"alice-row", "bob-row", "catalog-row", "project-row", "system-row"}
	for _, tc := range []struct {
		name                  string
		authzr                authz.Authorizer
		plural, singular, who string
		status                int
		want                  []string
	}{
		{"own and shared rows", testRBAC(), "rate-limits", "rate-limit", "alice", 200, []string{"alice-row", "catalog-row", "system-row"}},
		{"another user's own and shared rows", testRBAC(), "rate-limits", "rate-limit", "bob", 200, []string{"bob-row", "catalog-row", "system-row"}},
		{"admin sees every row", testRBAC(), "rate-limits", "rate-limit", "root", 200, all},
		{"admin token sees every row", testRBAC(), "rate-limits", "rate-limit", "token", 200, all},
		{"a scoped kind with no binding lists own rows", testRBAC(), "policies", "policy", "alice", 200, []string{"alice-row", "catalog-row", "system-row"}},
		{"single mode filters nothing", authz.AlwaysAllowAuthenticated{}, "rate-limits", "rate-limit", "alice", 200, all},
		{"no actor", testRBAC(), "rate-limits", "rate-limit", "", 401, nil},
		{"single mode still needs an actor", authz.AlwaysAllowAuthenticated{}, "rate-limits", "rate-limit", "", 401, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newThingHarness(t, tc.authzr, tc.plural, tc.singular, noSettings{}, seedThings()...)
			w := scopeReq(t, h, tc.who, http.MethodGet, "/"+tc.plural, "")
			if tc.status != 200 {
				wantAPIError(t, w, tc.status, "", "unauthenticated")
				return
			}
			var out struct {
				Items []scopedThing `json:"items"`
				Total int           `json:"total"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 {
				t.Fatalf("list = %d (%v): %s", w.Code, err, w.Body)
			}
			var got []string
			for _, it := range out.Items {
				got = append(got, it.Meta.Name)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" || out.Total != len(tc.want) {
				t.Fatalf("total %d, names (-want +got):\n%s", out.Total, diff)
			}
		})
	}
}

// One table under both authorization modes: single drops scoping and keeps governance, rbac hides foreign rows as 404 and refuses shared ones with 403. A refused write leaves the store as it was.
func TestCRUDAccessByMode(t *testing.T) {
	const (
		rename     = `{"metadata":{"name":"renamed"}}`
		hostOwned  = `{"metadata":{"name":"shared","owner":{"kind":"host","id":"h-1"}}}`
		forbidden  = "forbidden"
		sysEdit    = "rate-limit is system-owned: only an admin may edit it"
		sysDelete  = "rate-limit is system-owned: only an admin may delete it"
		govDelete  = "delete of rate-limit is disabled on this relay; the kind has no governance settings section to enable it"
		byID       = "/rate-limits/by-id/"
		collection = "/rate-limits"
	)
	type outcome struct {
		status int
		msg    string
	}
	ok := func(status int) outcome { return outcome{status: status} }
	noRef := func(ref string) outcome { return outcome{404, fmt.Sprintf("rate-limit %q not found", ref)} }
	noID := func(id string) outcome { return outcome{404, fmt.Sprintf("rate-limit with id %q not found", id)} }

	for _, tc := range []struct {
		name, who, method, path, body string
		single, rbac                  outcome
	}{
		{"get own by id", "alice", "GET", collection + "/" + aliceID, "", ok(200), ok(200)},
		{"get own by slug", "alice", "GET", collection + "/alice-row", "", ok(200), ok(200)},
		{"get catalog row", "alice", "GET", collection + "/" + catalogID, "", ok(200), ok(200)},
		{"get system row", "alice", "GET", collection + "/" + systemID, "", ok(200), ok(200)},
		{"get foreign by id", "alice", "GET", collection + "/" + bobID, "", ok(200), noRef(bobID)},
		{"get foreign by slug", "alice", "GET", collection + "/bob-row", "", ok(200), noRef("bob-row")},
		{"get project row", "alice", "GET", collection + "/" + projectID, "", ok(200), noRef(projectID)},
		{"get unknown slug", "alice", "GET", collection + "/nope", "", noRef("nope"), noRef("nope")},
		{"get unknown id", "alice", "GET", collection + "/" + unknownID, "", noRef(unknownID), noRef(unknownID)},
		{"admin gets foreign", "root", "GET", collection + "/" + bobID, "", ok(200), ok(200)},
		{"admin token gets project row", "token", "GET", collection + "/" + projectID, "", ok(200), ok(200)},

		{"create personal", "alice", "POST", collection, `{"metadata":{"name":"mine"}}`, ok(201), ok(201)},
		{"create catalog-owned", "alice", "POST", collection, hostOwned, ok(201), outcome{403, forbidden}},
		{"admin creates catalog-owned", "root", "POST", collection, hostOwned, ok(201), ok(201)},

		{"update own", "alice", "PUT", byID + aliceID, rename, ok(200), ok(200)},
		{"update foreign", "alice", "PUT", byID + bobID, rename, ok(200), noID(bobID)},
		{"update project row", "alice", "PUT", byID + projectID, rename, ok(200), noID(projectID)},
		{"update catalog row", "alice", "PUT", byID + catalogID, rename, ok(200), outcome{403, forbidden}},
		{"update system row", "alice", "PUT", byID + systemID, rename, outcome{403, sysEdit}, outcome{403, forbidden}},
		{"update unknown id", "root", "PUT", byID + unknownID, rename, noID(unknownID), noID(unknownID)},
		{"admin updates foreign", "root", "PUT", byID + bobID, rename, ok(200), ok(200)},
		{"admin updates system row", "root", "PUT", byID + systemID, rename, ok(200), ok(200)},
		{"admin token updates system row", "token", "PUT", byID + systemID, rename, ok(200), ok(200)},

		{"delete own", "alice", "DELETE", byID + aliceID, "", ok(204), ok(204)},
		{"delete foreign", "alice", "DELETE", byID + bobID, "", ok(204), noID(bobID)},
		{"delete system row", "alice", "DELETE", byID + systemID, "", outcome{403, sysDelete}, outcome{403, forbidden}},
		{"delete unknown id", "root", "DELETE", byID + unknownID, "", noID(unknownID), noID(unknownID)},
		{"admin deletes foreign", "root", "DELETE", byID + bobID, "", ok(204), ok(204)},
		{"admin deletes system row", "root", "DELETE", byID + systemID, "", ok(204), ok(204)},
		{"admin token deletes system row", "token", "DELETE", byID + systemID, "", ok(204), ok(204)},
		// Governance is not an authorization mode: a catalog-managed row's delete is off for an admin too.
		{"delete catalog row", "alice", "DELETE", byID + catalogID, "", outcome{403, govDelete}, outcome{403, forbidden}},
		{"admin deletes catalog row", "root", "DELETE", byID + catalogID, "", outcome{403, govDelete}, outcome{403, govDelete}},
	} {
		for mode, want := range map[string]outcome{"single": tc.single, "rbac": tc.rbac} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				var authzr authz.Authorizer = authz.AlwaysAllowAuthenticated{}
				if mode == "rbac" {
					authzr = testRBAC()
				}
				h, store := newScopeHarness(t, authzr, seedThings()...)
				before := newMemStore(thingMeta, seedThings()...).items
				w := scopeReq(t, h, tc.who, tc.method, tc.path, tc.body)
				if want.msg != "" {
					wantAPIError(t, w, want.status, "", want.msg)
					if diff := cmp.Diff(before, store.items); diff != "" {
						t.Fatalf("a refused %s changed the store (-before +after):\n%s", tc.method, diff)
					}
					return
				}
				if w.Code != want.status {
					t.Fatalf("status = %d, want %d: %s", w.Code, want.status, w.Body)
				}
				id := tc.path[strings.LastIndex(tc.path, "/")+1:]
				switch tc.method {
				case "GET":
					if got := decodeMeta(t, w, 200); got.ID != id && got.Name != id {
						t.Fatalf("got row %s (%s), want %s", got.ID, got.Name, id)
					}
				case "POST":
					if len(store.items) != len(before)+1 {
						t.Fatalf("store holds %d rows, want one more than %d", len(store.items), len(before))
					}
				case "PUT":
					want := *before[id]
					want.Meta.Name, want.Meta.Dirty = "renamed", true
					if diff := cmp.Diff(&want, store.items[id]); diff != "" {
						t.Fatalf("stored row (-want +got):\n%s", diff)
					}
				case "DELETE":
					if _, still := store.items[id]; still || len(store.items) != len(before)-1 {
						t.Fatalf("row %s still stored, or more than it went: %d rows", id, len(store.items))
					}
				}
			})
		}
	}
}

// The governance section of a kind decides edits and deletes of its catalog-managed rows; tenant rows never consult it.
func TestCRUDGovernanceSectionDecidesCatalogRows(t *testing.T) {
	locked := govSettings{"governance:rate-limit": {AllowEdit: false, AllowDelete: true}}
	h, store := newThingHarness(t, testRBAC(), "rate-limits", "rate-limit", locked, seedThings()...)

	w := scopeReq(t, h, "root", "PUT", "/rate-limits/by-id/"+catalogID, `{"metadata":{"name":"renamed"}}`)
	wantAPIError(t, w, 403, "", "edit of rate-limit is disabled on this relay; enable it via the governance:rate-limit settings section")
	if store.items[catalogID].Meta.Name != "catalog-row" {
		t.Fatalf("the refused edit was stored: %+v", store.items[catalogID].Meta)
	}
	if w := scopeReq(t, h, "alice", "PUT", "/rate-limits/by-id/"+aliceID, `{"metadata":{"name":"renamed"}}`); w.Code != 200 {
		t.Fatalf("edit of a user-owned row = %d, want 200: %s", w.Code, w.Body)
	}
	if w := scopeReq(t, h, "root", "DELETE", "/rate-limits/by-id/"+catalogID, ""); w.Code != 204 || store.items[catalogID] != nil {
		t.Fatalf("delete with allowDelete = %d, row %v: %s", w.Code, store.items[catalogID], w.Body)
	}

	// A row the catalog no longer ships is the operator's: the section stops governing it.
	at := time.Unix(1_700_000_000, 0).UTC()
	detached := seedThings()[2]
	detached.Meta.CatalogDetachedAt = &at
	h, _ = newThingHarness(t, testRBAC(), "rate-limits", "rate-limit", locked, detached)
	if got := decodeMeta(t, scopeReq(t, h, "root", "PUT", "/rate-limits/by-id/"+catalogID, `{"metadata":{"name":"renamed"}}`), 200); got.Name != "renamed" || got.CatalogDetachedAt == nil || !got.CatalogDetachedAt.Equal(at) {
		t.Fatalf("edit of a detached row = %+v, want renamed and still detached", got)
	}
}

// Create stamps the id, derives a free slug and settles the owner before anything is authorized; a refused body stores nothing.
func TestCRUDCreate(t *testing.T) {
	const reserved = "owner.kind=system is reserved for seed; omit owner.kind on create"
	org, alice := meta.Global, meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}
	rbmeta := func(b *rolebinding.RoleBinding) *meta.Metadata { return &b.Meta }
	roleBindings := newMemStore(rbmeta)
	things := newMemStore(thingMeta, &scopedThing{Meta: meta.Metadata{ID: aliceID, Name: "my-limit", Owner: alice}})
	teams := newMemStore(func(v *team.Team) *meta.Metadata { return &v.Meta })
	groups := newMemStore(func(v *group.Group) *meta.Metadata { return &v.Meta })
	roles := newMemStore(func(v *role.Role) *meta.Metadata { return &v.Meta })
	stored := func() int {
		return len(things.items) + len(teams.items) + len(groups.items) + len(roles.items) + len(roleBindings.items)
	}

	h, api := testAPI(t, scopeActors)
	mountKind[scopedThing](api, "rate-limits", "rate-limit", things, testRBAC(), thingMeta, nil, meta.OwnerUser, nil, noSettings{})
	mountKind[team.Team](api, "teams", "team", teams, testRBAC(), teams.metaOf, (*team.Team).Validate, meta.OwnerOrganization, nil, noSettings{})
	mountKind[group.Group](api, "groups", "group", groups, testRBAC(), groups.metaOf, (*group.Group).Validate, meta.OwnerOrganization, nil, noSettings{})
	mountKind[role.Role](api, "roles", "role", roles, testRBAC(), roles.metaOf, (*role.Role).Validate, meta.OwnerOrganization, nil, noSettings{})
	mountKind[rolebinding.RoleBinding](api, "role-bindings", "role-binding", roleBindings, testRBAC(), rbmeta,
		func(b *rolebinding.RoleBinding) error { b.StampOwner(); return b.Validate() }, "", nil, noSettings{})

	const (
		tenant      = `{"metadata":{"name":"eng","displayName":"Eng"},"spec":{}}`
		personal    = `{"metadata":{"name":"eng","displayName":"Eng","owner":{"kind":"user"}},"spec":{}}`
		customRole  = `{"metadata":{"name":"release-manager","displayName":"RM"},"spec":{"rules":[{"kinds":["keys"],"verbs":["get"]}]}}`
		globalGrant = `{"metadata":{"name":"ops-admin","owner":{"kind":"system"}},"spec":{"roleId":"00000000-0000-7000-8000-000000000001","scope":{"kind":"system"},"subjects":[{"kind":"user","id":"00000000-0000-7000-8000-000000000002"}]}}`
		systemTeam  = `{"metadata":{"name":"ops","owner":{"kind":"system"}},"spec":{}}`
		systemRole  = `{"metadata":{"name":"ops-role","owner":{"kind":"system"}},"spec":{"rules":[{"kinds":["keys"],"verbs":["get"]}]}}`
	)
	for _, tc := range []struct {
		name, who, path, body string
		status                int
		msg                   string // refusals
		wantName              string
		wantOwner             meta.Owner
	}{
		{"a user's row is their own", "alice", "/rate-limits", `{"metadata":{"name":"mine"}}`, 201, "", "mine", alice},
		{"a truthful owner id is kept", "alice", "/rate-limits", `{"metadata":{"name":"mine2","owner":{"kind":"user","id":"u-alice"}}}`, 201, "", "mine2", alice},
		{"another user's id is refused", "alice", "/rate-limits", `{"metadata":{"name":"theirs","owner":{"kind":"user","id":"u-bob"}}}`, 400, "owner.id must be empty or match the calling user", "", meta.Owner{}},
		{"the admin token may name any user", "token", "/rate-limits", `{"metadata":{"name":"for-bob","owner":{"kind":"user","id":"u-bob"}}}`, 201, "", "for-bob", meta.Owner{Kind: meta.OwnerUser, ID: "u-bob"}},
		{"the admin token names no user, so its row is the organization's", "token", "/rate-limits", `{"metadata":{"name":"ci-row"}}`, 201, "", "ci-row", org},
		{"an empty name is derived from displayName", "alice", "/rate-limits", `{"metadata":{"name":"","displayName":"Burst Cap!"}}`, 201, "", "burst-cap", alice},
		{"a derived slug steps past a taken one", "alice", "/rate-limits", `{"metadata":{"name":"","displayName":"My Limit"}}`, 201, "", "my-limit-2", alice},
		{"no name at all falls back to the kind", "alice", "/rate-limits", `{"metadata":{"name":""}}`, 201, "", "rate-limit", alice},
		// A body from before the organization owner: system names it.
		{"system on a moved kind is the organization", "root", "/rate-limits", `{"metadata":{"name":"sys","owner":{"kind":"system"}}}`, 201, "", "sys", meta.Global},
		{"a catalog owner needs a grant", "alice", "/rate-limits", `{"metadata":{"name":"shared","owner":{"kind":"host","id":"h-1"}}}`, 403, "forbidden", "", meta.Owner{}},
		{"an admin creates a catalog-owned row", "root", "/rate-limits", `{"metadata":{"name":"shared","owner":{"kind":"host","id":"h-1"}}}`, 201, "", "shared", meta.Owner{Kind: meta.OwnerHost, ID: "h-1"}},

		// Team, Group and Role default to the organization, so the personal-row rule never carries a plain user through.
		{"a user creates no team", "alice", "/teams", tenant, 403, "forbidden", "", meta.Owner{}},
		{"a user creates no group", "alice", "/groups", tenant, 403, "forbidden", "", meta.Owner{}},
		{"a user creates no role", "alice", "/roles", customRole, 403, "forbidden", "", meta.Owner{}},
		{"an admin's team is the organization's", "root", "/teams", tenant, 201, "", "eng", org},
		{"an admin's group is the organization's", "root", "/groups", tenant, 201, "", "eng", org},
		{"an admin's role is the organization's", "root", "/roles", customRole, 201, "", "release-manager", org},
		{"a team written as the system's is the organization's", "root", "/teams", systemTeam, 201, "", "ops", org},
		{"a team needs no spec", "root", "/teams", `{"metadata":{"name":"bare-team"}}`, 201, "", "bare-team", org},
		{"a group needs no spec", "root", "/groups", `{"metadata":{"name":"bare-group"}}`, 201, "", "bare-group", org},
		{"a custom role is not the system's", "root", "/roles", systemRole, 400, reserved, "", meta.Owner{}},
		{"a team takes no personal owner", "alice", "/teams", personal, 400, "team owner.kind must be organization; omit metadata.owner", "", meta.Owner{}},
		{"a team takes no personal owner from an admin", "root", "/teams", personal, 400, "team owner.kind must be organization; omit metadata.owner", "", meta.Owner{}},
		{"a group takes no personal owner", "alice", "/groups", personal, 400, "group owner.kind must be organization; omit metadata.owner", "", meta.Owner{}},
		{"a group takes no personal owner from an admin", "root", "/groups", personal, 400, "group owner.kind must be organization; omit metadata.owner", "", meta.Owner{}},
		// A role binding's owner mirrors its scope; the global scope written {kind: system} is the organization.
		{"a global role binding is not a reserved owner", "root", "/role-bindings", globalGrant, 201, "", "ops-admin", org},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := stored()
			w := scopeReq(t, h, tc.who, "POST", tc.path, tc.body)
			if tc.msg != "" {
				wantAPIError(t, w, tc.status, "", tc.msg)
				if stored() != before {
					t.Fatal("a refused create stored a row")
				}
				return
			}
			got := decodeMeta(t, w, tc.status)
			if got.Name != tc.wantName || got.Owner != tc.wantOwner || !ids.Valid(got.ID) || stored() != before+1 {
				t.Fatalf("created %+v, want name %q owner %+v with a fresh id", got, tc.wantName, tc.wantOwner)
			}
		})
	}

	t.Run("an omitted name is derived from displayName", func(t *testing.T) {
		if got := decodeMeta(t, scopeReq(t, h, "alice", "POST", "/rate-limits", `{"metadata":{"displayName":"Burst"}}`), 201); got.Name != "burst" {
			t.Fatalf("name = %q, want burst", got.Name)
		}
	})
	t.Run("the body's id, version and detach mark are discarded", func(t *testing.T) {
		got := decodeMeta(t, scopeReq(t, h, "alice", "POST", "/rate-limits",
			`{"metadata":{"id":"`+bobID+`","name":"sneaky","resourceVersion":"7","catalogDetachedAt":"2026-01-01T00:00:00Z"}}`), 201)
		if got.ID == bobID || got.ResourceVersion != "" || got.CatalogDetachedAt != nil {
			t.Fatalf("created %+v, want a server id, no version and no detach mark", got)
		}
	})
	// A team's cap is a Budget its policy links, so a stray spec.budget is refused rather than dropped.
	t.Run("an unknown spec field is a 422 naming it", func(t *testing.T) {
		wantFieldError(t, scopeReq(t, h, "root", "POST", "/teams", `{"metadata":{"name":"capped"},"spec":{"budget":{"amount":"100"}}}`), "body.spec.budget", "unexpected property")
	})
}

// An update keeps what the body may not change: the path id, the stored owner unless the body names another, and the detach mark. A stale version is the caller's conflict.
func TestCRUDUpdate(t *testing.T) {
	alice, bob := meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}, meta.Owner{Kind: meta.OwnerUser, ID: "u-bob"}
	stale := fmt.Sprintf("rate-limit %q changed since you opened it; reload it and apply your change again", aliceID)
	for _, tc := range []struct {
		name, who, body string
		status          int
		code, msg       string
		wantOwner       meta.Owner
	}{
		{"the path id wins over the body's", "alice", `{"metadata":{"id":"` + bobID + `","name":"renamed"}}`, 200, "", "", alice},
		{"the current version applies", "alice", `{"metadata":{"name":"renamed","resourceVersion":"3"}}`, 200, "", "", alice},
		{"a stale version is a conflict", "alice", `{"metadata":{"name":"renamed","resourceVersion":"2"}}`, 409, "stale_resource_version", stale, alice},
		{"an owner echoed without its id keeps the stored one", "root", `{"metadata":{"name":"renamed","owner":{"kind":"user"}}}`, 200, "", "", alice},
		{"an owner echoed whole keeps the stored one", "root", `{"metadata":{"name":"renamed","owner":{"kind":"user","id":"u-alice"}}}`, 200, "", "", alice},
		{"a user cannot hand their row to another", "alice", `{"metadata":{"name":"renamed","owner":{"kind":"user","id":"u-bob"}}}`, 400, "", "owner.id must be empty or match the calling user", alice},
		{"an admin moves a row to another user", "token", `{"metadata":{"name":"renamed","owner":{"kind":"user","id":"u-bob"}}}`, 200, "", "", bob},
		{"a move needs the create grant in the new scope", "alice", `{"metadata":{"name":"renamed","owner":{"kind":"host","id":"h-1"}}}`, 403, "", "forbidden", alice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seed := &scopedThing{Meta: meta.Metadata{ID: aliceID, Name: "alice-row", Owner: alice, ResourceVersion: "3"}}
			h, store := newScopeHarness(t, testRBAC(), seed)
			w := scopeReq(t, h, tc.who, "PUT", "/rate-limits/by-id/"+aliceID, tc.body)
			if tc.status != 200 {
				wantAPIError(t, w, tc.status, tc.code, tc.msg)
				if store.items[aliceID] != seed || len(store.items) != 1 {
					t.Fatalf("a refused update changed the store: %+v", store.items)
				}
				return
			}
			got := decodeMeta(t, w, 200)
			if got.ID != aliceID || got.Name != "renamed" || got.Owner != tc.wantOwner || !got.Dirty || len(store.items) != 1 {
				t.Fatalf("updated %+v in a store of %d, want %s renamed, dirty, owned by %+v", got, len(store.items), aliceID, tc.wantOwner)
			}
		})
	}
	// Only create derives an omitted name.
	t.Run("an omitted name is a 422 naming it", func(t *testing.T) {
		h, _ := newScopeHarness(t, testRBAC(), &scopedThing{Meta: meta.Metadata{ID: aliceID, Name: "alice-row", Owner: alice}})
		wantFieldError(t, scopeReq(t, h, "alice", "PUT", "/rate-limits/by-id/"+aliceID, `{"metadata":{"displayName":"Burst"}}`), "body.metadata", "expected required property name to be present")
	})
}

// failingStore fails every write with err.
type failingStore struct {
	*memStore[group.Group]
	err error
}

func (s failingStore) Upsert(context.Context, *group.Group) error { return s.err }
func (s failingStore) Delete(context.Context, string) error       { return s.err }

// A write the store refuses answers by what the refusal is: a row its provisioning source manages, a stale version, a refused grant and a bound account are the caller's; a delete failing any other way answers as a missing row, with the reason.
func TestCRUDStoreWriteErrors(t *testing.T) {
	const id = "01a12c00-0000-7000-8000-000000000001"
	const managedMsg = "group is managed by scim-okta; apply with force to override"
	managed := &directory.ManagedError{Kind: "group", Name: "ops", Source: "scim-okta"}
	gone := func(err error) string { return fmt.Sprintf("group with id %q not found: %s", id, err) }
	pgDown, stale := fmt.Errorf("pg down"), fmt.Errorf("upsert: %w", meta.ErrStaleResourceVersion)
	refused, bound := fmt.Errorf("grant: %w", authz.ErrForbidden), fmt.Errorf("%w: account a-1", policybinding.ErrAccountBound)
	for _, tc := range []struct {
		name            string
		err             error
		put             int
		putCode, putMsg string
		del             int
		delCode, delMsg string
	}{
		{"managed by a source", meta.FromDirectoryError(managed), 409, "managed_by_source", managedMsg, 409, "managed_by_source", managedMsg},
		{"managed by a source, unwrapped", managed, 409, "managed_by_source", managedMsg, 409, "managed_by_source", managedMsg},
		{"stale version at the store", stale, 409, "stale_resource_version",
			fmt.Sprintf("group %q changed since you opened it; reload it and apply your change again", id), 404, "", gone(stale)},
		{"refused grant", refused, 403, "", refused.Error(), 404, "", gone(refused)},
		{"account already bound", bound, 409, refcheck.CodeDuplicateBinding, bound.Error(), 404, "", gone(bound)},
		{"a status error passes through a delete", huma.Error409Conflict("raced"), 500, "internal_error", "raced", 409, "", "raced"},
		{"anything else", pgDown, 500, "internal_error", "pg down", 404, "", gone(pgDown)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gmeta := func(g *group.Group) *meta.Metadata { return &g.Meta }
			seed := &group.Group{Meta: meta.Metadata{ID: id, Name: "ops", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
			store := failingStore{newMemStore(gmeta, seed), tc.err}
			h, api := testAPI(t, scopeActors)
			mountKind[group.Group](api, "groups", "group", store, testRBAC(), gmeta, (*group.Group).Validate, meta.OwnerOrganization, nil, noSettings{})

			wantAPIError(t, scopeReq(t, h, "root", "PUT", "/groups/by-id/"+id, `{"metadata":{"name":"ops","displayName":"Ops 2"},"spec":{}}`), tc.put, tc.putCode, tc.putMsg)
			wantAPIError(t, scopeReq(t, h, "root", "DELETE", "/groups/by-id/"+id, ""), tc.del, tc.delCode, tc.delMsg)
			if w := scopeReq(t, h, "root", "POST", "/groups", `{"metadata":{"name":"new"},"spec":{}}`); w.Code != tc.put {
				t.Fatalf("create = %d, want %d: %s", w.Code, tc.put, w.Body)
			}
			if store.items[id] != seed || len(store.items) != 1 {
				t.Fatalf("store changed: %+v", store.items)
			}
		})
	}
}

// catalogEditorRBAC is the enforcing authorizer over the built-in roles and one binding: catalog-editor at the global scope for editorID.
func catalogEditorRBAC(t *testing.T, editorID string) authz.RBAC {
	t.Helper()
	builtins, err := role.Builtins()
	if err != nil {
		t.Fatalf("built-in roles: %v", err)
	}
	yes := true
	var editorRoleID string
	for _, r := range builtins {
		r.Meta.ID = ids.New()
		r.Spec.Enabled = &yes
		if r.Meta.Name == "catalog-editor" {
			editorRoleID = r.Meta.ID
		}
	}
	global := meta.Owner{Kind: meta.OwnerSystem}
	rb := &rolebinding.RoleBinding{Meta: meta.Metadata{ID: ids.New(), Name: "editor", Owner: global}}
	rb.Spec.RoleID = editorRoleID
	rb.Spec.Scope = global
	rb.Spec.Subjects = []rolebinding.Subject{{Kind: rolebinding.SubjectUser, ID: editorID}}
	rb.Spec.Enabled = &yes

	cat := catalogtest.Catalog{Roles: builtins, RoleBindings: []*rolebinding.RoleBinding{rb}}.Load(t)
	return authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
}

// newSharedCatalogHarness mounts hosts and host bindings, plus the kinds that keep personal rows, as registerCRUD does. nobody holds no role, editor is a catalog-editor.
func newSharedCatalogHarness(t *testing.T) (http.Handler, *actor.Actor) {
	t.Helper()
	editorID, nobodyID := ids.New(), ids.New()
	nobody := &actor.Actor{UserID: nobodyID, Username: "nobody", Subjects: appcatalog.UserSubjects(nobodyID, nil, nil)}
	rbac := catalogEditorRBAC(t, editorID)
	d := Deps{Authz: rbac}
	r, api := testAPI(t, map[string]*actor.Actor{
		"nobody": nobody,
		"editor": {UserID: editorID, Username: "editor", Subjects: appcatalog.UserSubjects(editorID, nil, nil)},
		"token":  {AdminToken: true, Username: "admin-token"},
	})
	hmeta := func(h *host.Host) *meta.Metadata { return &h.Meta }
	mountKind[host.Host](api, "hosts", "host", newMemStore(hmeta), rbac, hmeta, (*host.Host).Validate, "", nil, noSettings{})
	bmeta := func(b *binding.Binding) *meta.Metadata { return &b.Meta }
	mountKind[binding.Binding](api, "host-bindings", "host-binding", newMemStore(bmeta), rbac, bmeta, (*binding.Binding).Validate, "", nil, noSettings{})
	kmeta := func(k *hostkey.HostKey) *meta.Metadata { return &k.Meta }
	mountKind[hostkey.HostKey](api, "host-keys", "host-key", newMemStore(kmeta), rbac, kmeta, (*hostkey.HostKey).Validate, meta.OwnerUser, guardHostKey(d), noSettings{})
	rlmeta := func(r *ratelimit.RateLimit) *meta.Metadata { return &r.Meta }
	mountKind[ratelimit.RateLimit](api, "rate-limits", "rate-limit", newMemStore(rlmeta), rbac, rlmeta, (*ratelimit.RateLimit).Validate, meta.OwnerUser, nil, noSettings{})
	polmeta := func(p *policy.Policy) *meta.Metadata { return &p.Meta }
	mountKind[policy.Policy](api, "policies", "policy", newMemStore(polmeta), rbac, polmeta, (*policy.Policy).Validate, meta.OwnerUser, guardPolicyModels(d, nil), noSettings{})
	return r, nobody
}

func hostBody(name, owner string) string {
	return fmt.Sprintf(`{"metadata":{"name":%q%s},"spec":{"baseURL":"https://upstream.example"}}`, name, owner)
}

func bindingBody(name, owner string) string {
	return fmt.Sprintf(`{"metadata":{"name":%q%s},"spec":{"modelId":%q,"hostId":%q,"adapter":"openai"}}`,
		name, owner, ids.New(), ids.New())
}

// A host or binding is shared catalog data: a body naming a user owner is a bad request for any caller, and writing one takes a role.
func TestSharedCatalogKinds(t *testing.T) {
	const userOwner = `,"owner":{"kind":"user"}`
	for _, tc := range []struct {
		plural, singular string
		body             func(name, owner string) string
	}{
		{"hosts", "host", hostBody},
		{"host-bindings", "host-binding", bindingBody},
	} {
		t.Run(tc.plural, func(t *testing.T) {
			h, _ := newSharedCatalogHarness(t)
			path := "/" + tc.plural
			personal := tc.singular + " is shared catalog data and cannot be owned by a user; omit metadata.owner"
			for _, who := range []string{"nobody", "editor", "token"} {
				wantAPIError(t, scopeReq(t, h, who, "POST", path, tc.body("mine", userOwner)), 400, "", personal)
			}
			wantAPIError(t, scopeReq(t, h, "nobody", "POST", path, tc.body("nobodys", "")), 403, "", "forbidden")

			for _, who := range []string{"editor", "token"} {
				created := decodeMeta(t, scopeReq(t, h, who, "POST", path, tc.body(who+"-row", "")), 201)
				if created.Owner != (meta.Owner{}) {
					t.Fatalf("%s created a row owned by %+v, want no owner", who, created.Owner)
				}
				byID := path + "/by-id/" + created.ID
				wantAPIError(t, scopeReq(t, h, who, "PUT", byID, tc.body(who+"-row", userOwner)), 400, "", personal)
				// A row created with no owner is not readable without a role either, so the refusal answers as absent.
				hidden := fmt.Sprintf("%s with id %q not found", tc.singular, created.ID)
				wantAPIError(t, scopeReq(t, h, "nobody", "PUT", byID, tc.body("taken", "")), 404, "", hidden)
				wantAPIError(t, scopeReq(t, h, "nobody", "DELETE", byID, ""), 404, "", hidden)
				if got := decodeMeta(t, scopeReq(t, h, who, "PUT", byID, tc.body(who+"-renamed", "")), 200); got.Name != who+"-renamed" {
					t.Fatalf("%s update stored name %q", who, got.Name)
				}
			}
		})
	}
}

// Host keys, rate limits and policies keep personal rows: a user with no role creates them as their own.
func TestPersonalKindsAreCreatedAsTheCallersOwn(t *testing.T) {
	h, nobody := newSharedCatalogHarness(t)
	for plural, body := range map[string]string{
		"host-keys":   fmt.Sprintf(`{"metadata":{"name":"my-key"},"spec":{"hostId":%q,"accountId":%q,"valueFrom":{"kind":"stored"},"value":"sk-test"}}`, ids.New(), ids.New()),
		"rate-limits": `{"metadata":{"name":"my-limit"},"spec":{"rules":[{"meter":"requests","amount":10,"window":60,"strategy":"token-bucket"}]}}`,
		"policies":    `{"metadata":{"name":"my-policy"},"spec":{}}`,
	} {
		got := decodeMeta(t, scopeReq(t, h, "nobody", "POST", "/"+plural, body), 201)
		if want := (meta.Owner{Kind: meta.OwnerUser, ID: nobody.UserID}); got.Owner != want {
			t.Errorf("%s: owner = %+v, want %+v", plural, got.Owner, want)
		}
	}
}

// No caller, the admin token included, may point a key at relay's own configuration, and the refusal does not echo the variable.
func TestHostKeyRefusesReservedEnvRef(t *testing.T) {
	const reserved = `hostkey "env-key": valueFrom.env names a variable reserved for relay's own configuration (RELAY_*, WYOLET_*)`
	h, _ := newSharedCatalogHarness(t)
	hostID, accountID := ids.New(), ids.New()
	body := func(env string) string {
		return fmt.Sprintf(`{"metadata":{"name":"env-key"},"spec":{"hostId":%q,"accountId":%q,"valueFrom":{"kind":"env","env":%q}}}`, hostID, accountID, env)
	}
	wantAPIError(t, scopeReq(t, h, "token", "POST", "/host-keys", body("RELAY_MASTER_KEY")), 400, "", reserved)
	created := scopeReq(t, h, "token", "POST", "/host-keys", body("OPENAI_API_KEY"))
	if created.Code != 201 {
		t.Fatalf("create with an allowed env ref = %d: %s", created.Code, created.Body)
	}
	id := createdID(t, created.Body)
	for _, env := range []string{"RELAY_ADMIN_TOKEN", "WYOLET_OIDC_CLIENT_SECRET"} {
		wantAPIError(t, scopeReq(t, h, "token", "PUT", "/host-keys/by-id/"+id, body(env)), 400, "", reserved)
	}
	if w := scopeReq(t, h, "token", "PUT", "/host-keys/by-id/"+id, body("ACME_API_KEY")); w.Code != 200 {
		t.Errorf("update to an allowed env ref = %d: %s", w.Code, w.Body)
	}
}

// Every kind a person creates in a context lands in the caller's personal scope when the body names no owner: the UI has no owner picker.
func TestOmittedOwnerIsTheCallers(t *testing.T) {
	_, api := testAPI(t, nil)
	rows := registerCRUD(api, mountDeps(t), nil)
	for _, plural := range []string{"host-keys", "pools", "accounts", "budgets", "rate-limits", "policies", "policy-bindings"} {
		if got := rows[plural].defaultOwner; got != meta.OwnerUser {
			t.Errorf("%s: an omitted owner defaults to %q, want %q", plural, got, meta.OwnerUser)
		}
	}
}

// rescopeFixture is team T with projects P1 and P2. The caller is project-admin at P1 and holds secondRole at P2; P1 also holds one role binding and one service account the caller may update.
type rescopeFixture struct {
	h            http.Handler
	p1, p2       meta.Owner
	roleID       map[string]string
	victimID     string
	rbAtP1       *rolebinding.RoleBinding
	saAtP1       *serviceaccount.ServiceAccount
	roleBindings *memStore[rolebinding.RoleBinding]
	accounts     *memStore[serviceaccount.ServiceAccount]
}

func newRescopeFixture(t *testing.T, secondRole string) *rescopeFixture {
	t.Helper()
	yes := true
	teamID, p1ID, p2ID := meta.NewID(), meta.NewID(), meta.NewID()
	callerID, victimID := meta.NewID(), meta.NewID()

	builtins, err := role.Builtins()
	if err != nil {
		t.Fatal(err)
	}
	roleID := map[string]string{}
	rolesByID := map[string]*role.Role{}
	for _, r := range builtins {
		r.Meta.ID = meta.NewID()
		r.Spec.Enabled = &yes
		roleID[r.Meta.Name] = r.Meta.ID
		rolesByID[r.Meta.ID] = r
	}

	tm := &team.Team{Meta: meta.Metadata{ID: teamID, Name: "t", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	tm.Spec.Enabled = &yes
	mkProject := func(id, name string) *project.Project {
		p := &project.Project{Meta: meta.Metadata{ID: id, Name: name}}
		p.Spec.TeamID = teamID
		p.Spec.Enabled = &yes
		p.StampOwner()
		return p
	}
	projects := map[string]*project.Project{p1ID: mkProject(p1ID, "p1"), p2ID: mkProject(p2ID, "p2")}

	p1 := meta.Owner{Kind: meta.OwnerProject, ID: p1ID}
	p2 := meta.Owner{Kind: meta.OwnerProject, ID: p2ID}
	mkRB := func(name, rID string, scope meta.Owner, subject string) *rolebinding.RoleBinding {
		b := &rolebinding.RoleBinding{Meta: meta.Metadata{ID: meta.NewID(), Name: name}}
		b.Spec.RoleID = rID
		b.Spec.Scope = scope
		b.Spec.Subjects = []rolebinding.Subject{{Kind: rolebinding.SubjectUser, ID: subject}}
		b.Spec.Enabled = &yes
		b.StampOwner()
		return b
	}
	rbAtP1 := mkRB("viewer-p1", roleID["viewer"], p1, victimID)
	rbmeta := func(b *rolebinding.RoleBinding) *meta.Metadata { return &b.Meta }
	roleBindings := newMemStore(rbmeta, mkRB("caller-p1-admin", roleID["project-admin"], p1, callerID), mkRB("caller-p2", roleID[secondRole], p2, callerID), rbAtP1)
	sa := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "bot"}}
	sa.Spec.ProjectID = p1ID
	sa.StampOwner()
	sameta := func(s *serviceaccount.ServiceAccount) *meta.Metadata { return &s.Meta }
	accounts := newMemStore(sameta, sa)

	cat := catalogtest.Catalog{}.New()
	cat.UseTenancy(catalogtest.Rows[team.Team]{tm}, catalogtest.Rows[project.Project]{projects[p1ID], projects[p2ID]},
		accounts, catalogtest.Rows[group.Group](nil),
		catalogtest.Rows[role.Role](builtins), roleBindings, catalogtest.Rows[policybinding.PolicyBinding](nil))
	if err := cat.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	rbac := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
	checker := refcheck.Checker{Authz: rbac, Rows: refcheck.Lookup{
		Role:    func(_ context.Context, id string) *role.Role { return rolesByID[id] },
		Project: func(_ context.Context, id string) *project.Project { return projects[id] },
		Team: func(_ context.Context, id string) *team.Team {
			if id == teamID {
				return tm
			}
			return nil
		},
	}}

	caller := &actor.Actor{UserID: callerID, Username: "caller", Subjects: appcatalog.UserSubjects(callerID, nil, nil)}
	r, api := testAPI(t, map[string]*actor.Actor{"caller": caller})
	mountKind[rolebinding.RoleBinding](api, "role-bindings", "role-binding", roleBindings, rbac, rbmeta,
		func(b *rolebinding.RoleBinding) error { b.StampOwner(); return b.Validate() }, "",
		func(ctx context.Context, action string, _, incoming *rolebinding.RoleBinding) error {
			if action == "delete" || incoming == nil {
				return nil
			}
			return refErr(checker.RoleBinding(ctx, incoming))
		}, noSettings{})
	mountKind[serviceaccount.ServiceAccount](api, "service-accounts", "service-account", accounts, rbac, sameta,
		(*serviceaccount.ServiceAccount).Validate, meta.OwnerProject, guardServiceAccount(Deps{Authz: rbac}), noSettings{})
	return &rescopeFixture{h: r, p1: p1, p2: p2, roleID: roleID, victimID: victimID,
		rbAtP1: rbAtP1, saAtP1: sa, roleBindings: roleBindings, accounts: accounts}
}

func (f *rescopeFixture) roleBindingAtP2(roleName string) string {
	return `{"metadata":{"name":"viewer-p1","displayName":"Moved"},` +
		`"spec":{"roleId":"` + f.roleID[roleName] + `",` +
		`"scope":{"kind":"project","id":"` + f.p2.ID + `"},` +
		`"subjects":[{"kind":"user","id":"` + f.victimID + `"}]}}`
}

// A row whose owner follows a spec field moves with it, and the move takes the create grant in the new scope, as a POST of the same body would.
func TestUpdateIntoAnotherScopeNeedsCreateThere(t *testing.T) {
	t.Run("without create at the new scope", func(t *testing.T) {
		f := newRescopeFixture(t, "developer")
		wantAPIError(t, scopeReq(t, f.h, "caller", "POST", "/role-bindings", f.roleBindingAtP2("developer")), 403, "", "forbidden")
		wantAPIError(t, scopeReq(t, f.h, "caller", "PUT", "/role-bindings/by-id/"+f.rbAtP1.Meta.ID, f.roleBindingAtP2("developer")), 403, "", "forbidden")
		if got := f.roleBindings.items[f.rbAtP1.Meta.ID]; got != f.rbAtP1 || got.Spec.Scope != f.p1 || got.Meta.Owner != f.p1 {
			t.Fatalf("stored binding moved to %+v (owner %+v)", got.Spec.Scope, got.Meta.Owner)
		}
	})
	t.Run("with create at the new scope", func(t *testing.T) {
		f := newRescopeFixture(t, "project-admin")
		if w := scopeReq(t, f.h, "caller", "PUT", "/role-bindings/by-id/"+f.rbAtP1.Meta.ID, f.roleBindingAtP2("project-admin")); w.Code != 200 {
			t.Fatalf("move to P2 = %d, want 200: %s", w.Code, w.Body)
		}
		if got := f.roleBindings.items[f.rbAtP1.Meta.ID]; got.Spec.Scope != f.p2 || got.Meta.Owner != f.p2 {
			t.Fatalf("stored binding at %+v (owner %+v), want P2", got.Spec.Scope, got.Meta.Owner)
		}
	})
	// A service account's account and keys belong to its project, so it never moves, create grant or not.
	t.Run("a service account stays in its project", func(t *testing.T) {
		f := newRescopeFixture(t, "project-admin")
		byID := "/service-accounts/by-id/" + f.saAtP1.Meta.ID
		// With the stored owner kept, the new project contradicts it; naming the new owner too reaches the move refusal.
		wantAPIError(t, scopeReq(t, f.h, "caller", "PUT", byID, `{"metadata":{"name":"bot"},"spec":{"projectId":"`+f.p2.ID+`"}}`), 400, "",
			fmt.Sprintf("serviceaccount \"bot\": owner.id %q does not match spec.projectId %q", f.p1.ID, f.p2.ID))
		wantAPIError(t, scopeReq(t, f.h, "caller", "PUT", byID, `{"metadata":{"name":"bot","owner":{"kind":"project","id":"`+f.p2.ID+`"}},"spec":{"projectId":"`+f.p2.ID+`"}}`), 409, "resource_in_use",
			`service account "bot" stays in the project it was made in: its account and keys belong there; create a new service account in the other project`)
		if got := f.accounts.items[f.saAtP1.Meta.ID]; got != f.saAtP1 || got.Meta.Owner != f.p1 {
			t.Fatalf("stored account moved to %+v", got.Meta.Owner)
		}
	})
}
