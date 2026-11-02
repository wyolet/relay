package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

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

func TestPlanIssues_KindFilter(t *testing.T) {
	d := mountDeps(t)
	d.PlanView = emptyPlanView(t)
	r := chi.NewRouter()
	Mount(r, d)

	req := httptest.NewRequest(http.MethodGet, "/plan-issues?kind=key", nil)
	req.Header.Set("Authorization", "Bearer "+d.AdminToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /plan-issues?kind=key = %d: %s", w.Code, w.Body)
	}
}
