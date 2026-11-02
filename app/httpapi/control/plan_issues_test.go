package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/auth/apikey"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/engine/cell"
)

func emptyPlanView(t *testing.T) func() *engine.View {
	t.Helper()
	cells := cell.NewRegistry(cell.Options{})
	e := engine.New(engine.Options{Cells: cells})
	if err := e.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e.View
}

// orphanKeyView returns a view with one key whose user principal does not
// exist, which causes the engine to attach a ref_missing issue to the key.
func orphanKeyView(t *testing.T) (func() *engine.View, *key.Key) {
	t.Helper()
	userID := meta.NewID()
	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "orphan-key", Owner: meta.Owner{Kind: meta.OwnerUser, ID: userID}},
		Spec: key.Spec{
			Principal: key.Principal{Kind: key.PrincipalUser, ID: userID},
			KeyHash:   apikey.Hash("s3cr3t"),
		},
	}
	w := &plantest.World{}
	w.Keys = append(w.Keys, k)
	plans := w.Load(t)
	return plans.Engine.View, k
}

func TestPlanIssues_NilView(t *testing.T) {
	d := mountDeps(t)
	d.PlanView = nil
	r := chi.NewRouter()
	Mount(r, d)

	req := httptest.NewRequest(http.MethodGet, "/plan-issues", nil)
	req.Header.Set("Authorization", "Bearer "+d.AdminToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /plan-issues = %d: %s", w.Code, w.Body)
	}
	var items []PlanIssue
	if err := json.NewDecoder(w.Body).Decode(&items); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected empty slice, got %d items", len(items))
	}
}

func TestPlanIssues_EmptyView(t *testing.T) {
	d := mountDeps(t)
	d.PlanView = emptyPlanView(t)
	r := chi.NewRouter()
	Mount(r, d)

	req := httptest.NewRequest(http.MethodGet, "/plan-issues", nil)
	req.Header.Set("Authorization", "Bearer "+d.AdminToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /plan-issues = %d: %s", w.Code, w.Body)
	}
	var items []PlanIssue
	if err := json.NewDecoder(w.Body).Decode(&items); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected empty slice, got %d items", len(items))
	}
}

func TestPlanIssues_Unauthenticated(t *testing.T) {
	d := mountDeps(t)
	d.PlanView = emptyPlanView(t)
	r := chi.NewRouter()
	Mount(r, d)

	req := httptest.NewRequest(http.MethodGet, "/plan-issues", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /plan-issues = %d, want 401", w.Code)
	}
}

// TestPlanIssues_RealIssue builds a view where a key points at a missing user,
// producing a ref_missing issue, and asserts every returned field.
func TestPlanIssues_RealIssue(t *testing.T) {
	viewFunc, k := orphanKeyView(t)
	d := mountDeps(t)
	d.PlanView = viewFunc
	r := chi.NewRouter()
	Mount(r, d)

	req := httptest.NewRequest(http.MethodGet, "/plan-issues", nil)
	req.Header.Set("Authorization", "Bearer "+d.AdminToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /plan-issues = %d: %s", w.Code, w.Body)
	}
	var items []PlanIssue
	if err := json.NewDecoder(w.Body).Decode(&items); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("expected at least one issue, got none")
	}

	// Find our key's issue.
	var got *PlanIssue
	for i := range items {
		if items[i].ID == k.Meta.ID {
			got = &items[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("no issue for key %s (%s) among %v", k.Meta.Name, k.Meta.ID, items)
	}

	// Code and kind.
	if got.Code != plan.IssueRefMissing {
		t.Errorf("code = %q, want %q", got.Code, plan.IssueRefMissing)
	}
	if got.Kind != string(plan.KindKey) {
		t.Errorf("kind = %q, want %q", got.Kind, plan.KindKey)
	}
	if got.ID != k.Meta.ID {
		t.Errorf("id = %q, want %q", got.ID, k.Meta.ID)
	}
	// Name is the key's slug.
	if got.Name != k.Meta.Name {
		t.Errorf("name = %q, want %q", got.Name, k.Meta.Name)
	}
	// Severity: key can't serve → error.
	if got.Severity != "error" {
		t.Errorf("severity = %q, want error", got.Severity)
	}
	// Message is non-empty.
	if got.Message == "" {
		t.Error("message is empty")
	}
	// Refs: the missing principal ref.
	if len(got.Refs) == 0 {
		t.Error("refs is empty, want the missing principal ref")
	} else {
		ref := got.Refs[0]
		if ref.Kind == "" {
			t.Error("ref.kind is empty")
		}
		if ref.ID != k.Spec.Principal.ID {
			t.Errorf("ref.id = %q, want %q", ref.ID, k.Spec.Principal.ID)
		}
	}
}

// TestPlanIssues_KindFilter checks that kind= restricts results to that kind.
func TestPlanIssues_KindFilter(t *testing.T) {
	viewFunc, k := orphanKeyView(t)
	d := mountDeps(t)
	d.PlanView = viewFunc
	r := chi.NewRouter()
	Mount(r, d)

	// Filter to "key" — should see the issue.
	req := httptest.NewRequest(http.MethodGet, "/plan-issues?kind=key", nil)
	req.Header.Set("Authorization", "Bearer "+d.AdminToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /plan-issues?kind=key = %d: %s", w.Code, w.Body)
	}
	var items []PlanIssue
	if err := json.NewDecoder(w.Body).Decode(&items); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, it := range items {
		if it.Kind != "key" {
			t.Errorf("kind filter returned item with kind=%q", it.Kind)
		}
		if it.ID == k.Meta.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("expected key %s in kind=key results", k.Meta.ID)
	}

	// Filter to "policy" — should not see the key issue.
	req2 := httptest.NewRequest(http.MethodGet, "/plan-issues?kind=policy", nil)
	req2.Header.Set("Authorization", "Bearer "+d.AdminToken)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	var items2 []PlanIssue
	if err := json.NewDecoder(w2.Body).Decode(&items2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, it := range items2 {
		if it.ID == k.Meta.ID {
			t.Errorf("kind=policy returned the key issue")
		}
	}
}

// TestPlanIssues_KindIDFilter checks that kind= and id= together scope to one object.
func TestPlanIssues_KindIDFilter(t *testing.T) {
	viewFunc, k := orphanKeyView(t)
	d := mountDeps(t)
	d.PlanView = viewFunc
	r := chi.NewRouter()
	Mount(r, d)

	req := httptest.NewRequest(http.MethodGet, "/plan-issues?kind=key&id="+k.Meta.ID, nil)
	req.Header.Set("Authorization", "Bearer "+d.AdminToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /plan-issues?kind=key&id=... = %d: %s", w.Code, w.Body)
	}
	var items []PlanIssue
	if err := json.NewDecoder(w.Body).Decode(&items); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(items) == 0 {
		t.Fatalf("expected issues for key %s, got none", k.Meta.ID)
	}
	for _, it := range items {
		if it.ID != k.Meta.ID {
			t.Errorf("kind+id filter returned item for different object %s", it.ID)
		}
	}
}

// scopingAuthz is an Authorizer+Scoper that hides rows owned by hiddenOwner.
type scopingAuthz struct {
	authz.AlwaysAllowAuthenticated
	hiddenOwner meta.Owner
}

func (s scopingAuthz) Visible(_ context.Context, _, _ string, owner meta.Owner) bool {
	return owner != s.hiddenOwner
}

// TestPlanIssues_VisibilityScope checks that a caller whose authorizer hides a
// specific owner does not see issues on that owner's rows. Because the test
// authorizer is a Scoper (like RELAY_AUTHZ=rbac), owner-scope filtering applies.
func TestPlanIssues_VisibilityScope(t *testing.T) {
	viewFunc, k := orphanKeyView(t)

	// The key is owned by its user — set up a scoper that hides that owner.
	hiddenOwner := k.Meta.Owner

	d := mountDeps(t)
	d.PlanView = viewFunc
	d.Authz = scopingAuthz{hiddenOwner: hiddenOwner}
	r := chi.NewRouter()
	Mount(r, d)

	req := httptest.NewRequest(http.MethodGet, "/plan-issues", nil)
	req.Header.Set("Authorization", "Bearer "+d.AdminToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /plan-issues = %d: %s", w.Code, w.Body)
	}
	var items []PlanIssue
	if err := json.NewDecoder(w.Body).Decode(&items); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, it := range items {
		if it.ID == k.Meta.ID {
			t.Errorf("scoped caller sees issue for hidden key %s", k.Meta.ID)
		}
	}
}
