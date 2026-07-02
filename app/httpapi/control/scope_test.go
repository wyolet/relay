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

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/user"
)

// scopedThing is a minimal entity for exercising registerKind's owner
// scoping without dragging in a domain kind's validation or store.
type scopedThing struct {
	Meta meta.Metadata `json:"metadata"`
}

type memStore[T any] struct {
	metaOf func(*T) *meta.Metadata
	items  map[string]*T
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

type noSettings struct{}

func (noSettings) Setting(string) (any, bool) { return nil, false }

var scopeActors = map[string]*actor.Actor{
	"alice": {UserID: "u-alice", Username: "alice"},
	"bob":   {UserID: "u-bob", Username: "bob"},
	"root":  {UserID: "u-root", Username: "root", Roles: []string{user.RoleAdmin}},
	"token": {AdminToken: true, Username: "admin-token"},
}

// newScopeHarness mounts registerKind for one test kind behind a middleware
// that injects the actor named by the X-Test-Actor header.
func newScopeHarness(t *testing.T, authzr authz.Authorizer, seed ...*scopedThing) (http.Handler, *memStore[scopedThing]) {
	t.Helper()
	tmeta := func(v *scopedThing) *meta.Metadata { return &v.Meta }
	store := &memStore[scopedThing]{metaOf: tmeta, items: map[string]*scopedThing{}}
	for _, it := range seed {
		store.items[it.Meta.ID] = it
	}

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if a, ok := scopeActors[req.Header.Get("X-Test-Actor")]; ok {
				req = req.WithContext(actor.WithActor(req.Context(), a))
			}
			next.ServeHTTP(w, req)
		})
	})
	api := humachi.New(r, huma.DefaultConfig("scope-test", "0"))
	registerKind[scopedThing](
		api, "rate-limits", "rate-limit", store, authzr, tmeta,
		nil, // validate
		meta.OwnerUser,
		listScanResolver[scopedThing](store, tmeta),
		nil, nil, nil, nil,
		noSettings{},
		false,
		nil, // protect — actor injection above stands in for the auth chain
		nil, // filterSchema
	)
	return r, store
}

func scopeReq(t *testing.T, h http.Handler, who, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
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

func seedThings() []*scopedThing {
	return []*scopedThing{
		{Meta: meta.Metadata{ID: catalogID, Name: "catalog-row", Owner: meta.Owner{Kind: meta.OwnerHost, ID: "h-1"}}},
		{Meta: meta.Metadata{ID: aliceID, Name: "alice-row", Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}}},
		{Meta: meta.Metadata{ID: bobID, Name: "bob-row", Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-bob"}}},
		{Meta: meta.Metadata{ID: operatorID, Name: "operator-row", Owner: meta.Owner{Kind: meta.OwnerUser}}},
	}
}

const (
	catalogID  = "01950000-0000-7000-8000-0000000000c1"
	aliceID    = "01950000-0000-7000-8000-0000000000a1"
	bobID      = "01950000-0000-7000-8000-0000000000b1"
	operatorID = "01950000-0000-7000-8000-0000000000e1"
)

func TestOwnerScopedList(t *testing.T) {
	h, _ := newScopeHarness(t, authz.OwnerScoped{}, seedThings()...)

	tests := []struct {
		who       string
		wantNames []string
	}{
		{"alice", []string{"catalog-row", "alice-row"}},
		{"bob", []string{"catalog-row", "bob-row"}},
		{"root", []string{"catalog-row", "alice-row", "bob-row", "operator-row"}},
		{"token", []string{"catalog-row", "alice-row", "bob-row", "operator-row"}},
	}
	for _, tt := range tests {
		t.Run(tt.who, func(t *testing.T) {
			w := scopeReq(t, h, tt.who, http.MethodGet, "/rate-limits", "")
			if w.Code != http.StatusOK {
				t.Fatalf("list = %d, want 200: %s", w.Code, w.Body)
			}
			var out struct {
				Items []scopedThing `json:"items"`
				Total int           `json:"total"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(out.Items))
			for _, it := range out.Items {
				got = append(got, it.Meta.Name)
			}
			sort.Strings(got)
			want := append([]string(nil), tt.wantNames...)
			sort.Strings(want)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("list items = %v, want %v", got, want)
			}
			if out.Total != len(tt.wantNames) {
				t.Fatalf("total = %d, want %d (must reflect the scoped set)", out.Total, len(tt.wantNames))
			}
		})
	}
}

func TestOwnerScopedGet(t *testing.T) {
	h, _ := newScopeHarness(t, authz.OwnerScoped{}, seedThings()...)

	tests := []struct {
		name string
		who  string
		ref  string
		want int
	}{
		{"own row by id", "alice", aliceID, 200},
		{"own row by slug", "alice", "alice-row", 200},
		{"catalog row", "alice", catalogID, 200},
		{"foreign row hidden as 404", "alice", bobID, 404},
		{"foreign row by slug hidden as 404", "alice", "bob-row", 404},
		{"operator row hidden as 404", "alice", operatorID, 404},
		{"admin sees foreign row", "root", bobID, 200},
		{"admin token sees operator row", "token", operatorID, 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := scopeReq(t, h, tt.who, http.MethodGet, "/rate-limits/"+tt.ref, "")
			if w.Code != tt.want {
				t.Fatalf("get %s = %d, want %d: %s", tt.ref, w.Code, tt.want, w.Body)
			}
		})
	}
}

func TestOwnerScopedCreate(t *testing.T) {
	h, store := newScopeHarness(t, authz.OwnerScoped{})

	// A plain user create gets stamped user-owned with the caller's id.
	w := scopeReq(t, h, "alice", http.MethodPost, "/rate-limits", `{"metadata":{"name":"mine","displayName":"Mine"}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body)
	}
	var created scopedThing
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Meta.Owner.Kind != meta.OwnerUser || created.Meta.Owner.ID != "u-alice" {
		t.Fatalf("created owner = %+v, want user/u-alice", created.Meta.Owner)
	}

	// Creating a non-user-owned row is an admin operation.
	w = scopeReq(t, h, "alice", http.MethodPost, "/rate-limits", `{"metadata":{"name":"shared","owner":{"kind":"host","id":"h-1"}}}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("catalog-owned create by user = %d, want 403: %s", w.Code, w.Body)
	}
	w = scopeReq(t, h, "root", http.MethodPost, "/rate-limits", `{"metadata":{"name":"shared","owner":{"kind":"host","id":"h-1"}}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("catalog-owned create by admin = %d, want 201: %s", w.Code, w.Body)
	}
	if len(store.items) != 2 {
		t.Fatalf("store has %d rows, want 2", len(store.items))
	}
}

func TestOwnerScopedUpdateDelete(t *testing.T) {
	tests := []struct {
		name   string
		who    string
		method string
		id     string
		want   int
	}{
		{"update own", "alice", http.MethodPut, aliceID, 200},
		{"update foreign is 404", "alice", http.MethodPut, bobID, 404},
		{"update operator row is 404", "alice", http.MethodPut, operatorID, 404},
		{"update catalog row is 403", "alice", http.MethodPut, catalogID, 403},
		{"admin updates foreign", "root", http.MethodPut, bobID, 200},
		{"admin token updates operator row", "token", http.MethodPut, operatorID, 200},
		{"delete own", "alice", http.MethodDelete, aliceID, 204},
		{"delete foreign is 404", "alice", http.MethodDelete, bobID, 404},
		{"delete catalog row is 403", "alice", http.MethodDelete, catalogID, 403},
		{"admin deletes foreign", "root", http.MethodDelete, bobID, 204},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := newScopeHarness(t, authz.OwnerScoped{}, seedThings()...)
			body := ""
			if tt.method == http.MethodPut {
				body = `{"metadata":{"name":"renamed"}}`
			}
			w := scopeReq(t, h, tt.who, tt.method, "/rate-limits/by-id/"+tt.id, body)
			if w.Code != tt.want {
				t.Fatalf("%s %s = %d, want %d: %s", tt.method, tt.id, w.Code, tt.want, w.Body)
			}
		})
	}
}

// Single-user regression: with the default authorizer nothing is filtered
// or owner-gated — behavior identical to pre-scoping relays.
func TestAlwaysAllowUnscoped(t *testing.T) {
	h, _ := newScopeHarness(t, authz.AlwaysAllowAuthenticated{}, seedThings()...)

	w := scopeReq(t, h, "alice", http.MethodGet, "/rate-limits", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", w.Code, w.Body)
	}
	var out struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Total != 4 {
		t.Fatalf("total = %d, want 4 (unscoped)", out.Total)
	}

	if w := scopeReq(t, h, "alice", http.MethodGet, "/rate-limits/"+bobID, ""); w.Code != 200 {
		t.Fatalf("get foreign = %d, want 200", w.Code)
	}
	if w := scopeReq(t, h, "alice", http.MethodPut, "/rate-limits/by-id/"+bobID, `{"metadata":{"name":"renamed"}}`); w.Code != 200 {
		t.Fatalf("update foreign = %d, want 200: %s", w.Code, w.Body)
	}
	// Unauthenticated is still rejected.
	if w := scopeReq(t, h, "", http.MethodGet, "/rate-limits", ""); w.Code != 401 {
		t.Fatalf("unauthenticated list = %d, want 401", w.Code)
	}
}
