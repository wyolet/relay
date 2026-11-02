package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/actor"
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

// orphanKeyView returns a view with one key whose AccountID points to no
// existing account, causing the engine to attach a ref_missing issue to the key.
func orphanKeyView(t *testing.T) (func() *engine.View, *key.Key) {
	t.Helper()
	userID := meta.NewID()
	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "orphan-key", Owner: meta.Owner{Kind: meta.OwnerUser, ID: userID}},
		Spec: key.Spec{
			Principal: key.Principal{Kind: key.PrincipalUser, ID: userID},
			AccountID: meta.NewID(), // points to no account → ref_missing
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
	// Refs: the missing account ref.
	if len(got.Refs) == 0 {
		t.Error("refs is empty, want the missing account ref")
	} else {
		ref := got.Refs[0]
		if ref.Kind == "" {
			t.Error("ref.kind is empty")
		}
		if ref.ID != k.Spec.AccountID {
			t.Errorf("ref.id = %q, want %q", ref.ID, k.Spec.AccountID)
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

// TestIssueVisible_DerivedKind checks that issueVisible handles the singular==""
// (derived kind) path: without a Scoper the issue is always shown; with a
// Scoper only an admin may see it.
func TestIssueVisible_DerivedKind(t *testing.T) {
	id := meta.NewID()
	owner := meta.Owner{} // OwnerOf returns the zero value for derived kinds

	nonAdminCtx := actor.WithActor(context.Background(), &actor.Actor{UserID: meta.NewID(), Username: "user"})
	adminCtx := actor.WithActor(context.Background(), &actor.Actor{AdminToken: true, Username: "admin-token"})

	noScoper := authz.AlwaysAllowAuthenticated{}
	if !issueVisible(nonAdminCtx, noScoper, "", id, owner) {
		t.Error("no Scoper, non-admin: derived-kind issue should be visible")
	}
	if !issueVisible(adminCtx, noScoper, "", id, owner) {
		t.Error("no Scoper, admin: derived-kind issue should be visible")
	}

	scoper := scopingAuthz{} // satisfies authz.Scoper; hiddenOwner not relevant here
	if issueVisible(nonAdminCtx, scoper, "", id, owner) {
		t.Error("Scoper present, non-admin: derived-kind issue should NOT be visible")
	}
	if !issueVisible(adminCtx, scoper, "", id, owner) {
		t.Error("Scoper present, admin: derived-kind issue should be visible")
	}
}

// twoOrphanKeysView returns a view with two orphan keys owned by two different
// users; each key's AccountID points to a non-existent account.
func twoOrphanKeysView(t *testing.T) (func() *engine.View, *key.Key, *key.Key) {
	t.Helper()
	makeKey := func(name string) *key.Key {
		uid := meta.NewID()
		return &key.Key{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerUser, ID: uid}},
			Spec: key.Spec{
				Principal: key.Principal{Kind: key.PrincipalUser, ID: uid},
				AccountID: meta.NewID(),
				KeyHash:   apikey.Hash(name),
			},
		}
	}
	k1, k2 := makeKey("orphan-a"), makeKey("orphan-b")
	w := &plantest.World{}
	w.Keys = append(w.Keys, k1, k2)
	plans := w.Load(t)
	return plans.Engine.View, k1, k2
}

// TestPlanIssues_ScopedCallerSeesExactly checks that under a scoping authorizer a
// non-admin caller sees issues for the rows it may read and not for rows it may not.
// This confirms the endpoint is accessible to all authenticated callers (the old
// Authorize gate used a non-vocabulary kind and would 403 non-wildcard RBAC
// principals before per-row filtering could run).
func TestPlanIssues_ScopedCallerSeesExactly(t *testing.T) {
	viewFunc, k1, k2 := twoOrphanKeysView(t)

	// The scoper allows k1's owner but hides k2's owner.
	d := mountDeps(t)
	d.PlanView = viewFunc
	d.Authz = scopingAuthz{hiddenOwner: k2.Meta.Owner}
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

	seenK1, seenK2 := false, false
	for _, it := range items {
		if it.ID == k1.Meta.ID {
			seenK1 = true
		}
		if it.ID == k2.Meta.ID {
			seenK2 = true
		}
	}
	if !seenK1 {
		t.Errorf("scoped caller did not see issue for visible key %s", k1.Meta.ID)
	}
	if seenK2 {
		t.Errorf("scoped caller saw issue for hidden key %s", k2.Meta.ID)
	}
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
