//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/internal/auth"
	"github.com/wyolet/relay/internal/catalog"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/reqid"
)

const adminTestToken = "test-admin-secret"

func buildAdminTestServer(t *testing.T) (*httptest.Server, *catalog.PGStore) {
	t.Helper()
	ctx := context.Background()
	dsn := startPG(t)
	runMigrationsForTest(t, dsn)

	// Seed a minimal provider so the catalog validator is satisfied on Reload.
	seedSt := storagemod.MustOpenStorage(ctx, t, dsn)
	if err := storagemod.SeedProviderRow(ctx, seedSt,
		"seed-prov", `{"Name":"seed-prov"}`, `{"kind":"ollama","baseURL":"http://localhost:11434","default":true}`); err != nil {
		t.Fatalf("seed provider: %v", err)
	}

	st := storagemod.MustOpenStorage(ctx, t, dsn)
	store, err := catalog.NewPGStoreNoReload(st.Catalog, st)
	if err != nil {
		t.Fatalf("configstore: %v", err)
	}
	if err := store.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	r := chi.NewRouter()
	r.Use(reqid.Middleware(slog.Default()))
	r.Use(httpmw.LimitBody(httpmw.DefaultMaxRequestBytes))

	deps := crudDeps(st, store)
	kinds := buildAdminKinds(store, st)
	crudH := buildAdminCRUD(kinds, deps, store, nil)

	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	authMW := auth.Middleware(nil, nil) // no API-key auth needed for admin tests
	mountHuma(r, authMW, stub, stub, stub, stub, nil, crudH, adminTestToken)

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, store
}

func adminReq(t *testing.T, srv *httptest.Server, method, path string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequest(method, srv.URL+path, &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Relay-Admin-Token", adminTestToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func decodeResp(t *testing.T, resp *http.Response, out any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// --- Provider ---

func TestAdminProvider_CRUD(t *testing.T) {
	srv, store := buildAdminTestServer(t)

	body := map[string]any{
		"apiVersion": "relay.wyolet.dev/v1",
		"kind":       "Provider",
		"metadata":   map[string]string{"name": "test-prov"},
		"spec":       map[string]any{"kind": "ollama", "baseURL": "http://localhost:11434"},
	}

	// Create → 201
	resp := adminReq(t, srv, http.MethodPost, "/control/providers", body)
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("create: want 201 got %d", resp.StatusCode)
	}
	var created catalog.Provider
	decodeResp(t, resp, &created)
	if created.Metadata.Name != "test-prov" {
		t.Errorf("name: got %q", created.Metadata.Name)
	}

	// GET → 200
	resp = adminReq(t, srv, http.MethodGet, "/control/providers/test-prov", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: want 200 got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// List → includes new resource
	resp = adminReq(t, srv, http.MethodGet, "/control/providers", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: want 200 got %d", resp.StatusCode)
	}
	var listOut struct{ Items []catalog.Provider }
	decodeResp(t, resp, &listOut)
	found := false
	for _, p := range listOut.Items {
		if p.Metadata.Name == "test-prov" {
			found = true
		}
	}
	if !found {
		t.Error("list: created provider not in items")
	}

	// Update → 200
	body["spec"] = map[string]any{"kind": "openai", "baseURL": "https://api.openai.com"}
	resp = adminReq(t, srv, http.MethodPut, "/control/providers/test-prov", body)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("update: want 200 got %d", resp.StatusCode)
	}
	var updated catalog.Provider
	decodeResp(t, resp, &updated)
	if updated.Spec.Kind != "openai" {
		t.Errorf("update: want kind=openai, got %q", updated.Spec.Kind)
	}

	// Snapshot auto-reload: in-memory should reflect change
	p, ok := store.ProviderByName("test-prov")
	if !ok {
		t.Fatal("auto-reload: provider not in snapshot")
	}
	if p.Spec.Kind != "openai" {
		t.Errorf("auto-reload: want kind=openai, got %q", p.Spec.Kind)
	}

	// Delete → 204
	resp = adminReq(t, srv, http.MethodDelete, "/control/providers/test-prov", nil)
	if resp.StatusCode != http.StatusNoContent {
		resp.Body.Close()
		t.Fatalf("delete: want 204 got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// GET after delete → 404
	resp = adminReq(t, srv, http.MethodGet, "/control/providers/test-prov", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: want 404 got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- Policy ---

func TestAdminPool_CRUD(t *testing.T) {
	srv, store := buildAdminTestServer(t)

	// Insert a provider first (Policy.provider ref not validated by validate() unless strict)
	provBody := map[string]any{
		"apiVersion": "relay.wyolet.dev/v1",
		"kind":       "Provider",
		"metadata":   map[string]string{"name": "policy-prov"},
		"spec":       map[string]any{"kind": "ollama", "baseURL": "http://localhost:11434"},
	}
	resp := adminReq(t, srv, http.MethodPost, "/control/providers", provBody)
	resp.Body.Close()

	body := map[string]any{
		"apiVersion": "relay.wyolet.dev/v1",
		"kind":       "Policy",
		"metadata":   map[string]string{"name": "test-policy"},
		"spec":       map[string]any{"provider": "policy-prov"},
	}

	resp = adminReq(t, srv, http.MethodPost, "/control/policies", body)
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("create policy: want 201 got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = adminReq(t, srv, http.MethodGet, "/control/policies/test-policy", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get policy: want 200 got %d", resp.StatusCode)
	}
	resp.Body.Close()

	_, ok := store.PolicyByName("test-policy")
	if !ok {
		t.Fatal("auto-reload: policy not in snapshot")
	}

	resp = adminReq(t, srv, http.MethodDelete, "/control/policies/test-policy", nil)
	if resp.StatusCode != http.StatusNoContent {
		resp.Body.Close()
		t.Fatalf("delete policy: want 204 got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = adminReq(t, srv, http.MethodGet, "/control/policies/test-policy", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: want 404 got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- Model ---

func TestAdminModel_CRUD(t *testing.T) {
	srv, store := buildAdminTestServer(t)

	// Insert provider first
	provBody := map[string]any{
		"apiVersion": "relay.wyolet.dev/v1", "kind": "Provider",
		"metadata": map[string]string{"name": "model-prov"},
		"spec":     map[string]any{"kind": "ollama", "baseURL": "http://localhost:11434"},
	}
	resp := adminReq(t, srv, http.MethodPost, "/control/providers", provBody)
	resp.Body.Close()

	body := map[string]any{
		"apiVersion": "relay.wyolet.dev/v1",
		"kind":       "Model",
		"metadata":   map[string]string{"name": "test-model"},
		"spec":       map[string]any{"provider": "model-prov", "upstreamName": "llama3:8b"},
	}

	resp = adminReq(t, srv, http.MethodPost, "/control/models", body)
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("create model: want 201 got %d", resp.StatusCode)
	}
	resp.Body.Close()

	body["spec"] = map[string]any{"provider": "model-prov", "upstreamName": "llama3:70b"}
	resp = adminReq(t, srv, http.MethodPut, "/control/models/test-model", body)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("update model: want 200 got %d", resp.StatusCode)
	}
	var updated catalog.Model
	decodeResp(t, resp, &updated)
	if updated.Spec.UpstreamName != "llama3:70b" {
		t.Errorf("update model: want upstreamName=llama3:70b, got %q", updated.Spec.UpstreamName)
	}

	m, ok := store.ModelByName("test-model")
	if !ok {
		t.Fatal("auto-reload: model not in snapshot")
	}
	if m.Spec.UpstreamName != "llama3:70b" {
		t.Errorf("auto-reload: want upstreamName=llama3:70b, got %q", m.Spec.UpstreamName)
	}

	resp = adminReq(t, srv, http.MethodDelete, "/control/models/test-model", nil)
	if resp.StatusCode != http.StatusNoContent {
		resp.Body.Close()
		t.Fatalf("delete model: want 204 got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = adminReq(t, srv, http.MethodGet, "/control/models/test-model", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: want 404 got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- Route ---

func TestAdminRoute_CRUD(t *testing.T) {
	srv, store := buildAdminTestServer(t)

	// Insert a model so the route ref validates
	modelBody := map[string]any{
		"apiVersion": "relay.wyolet.dev/v1", "kind": "Model",
		"metadata": map[string]string{"name": "route-model"},
		"spec":     map[string]any{"provider": "seed-prov", "upstreamName": "llama3:8b"},
	}
	resp := adminReq(t, srv, http.MethodPost, "/control/models", modelBody)
	resp.Body.Close()

	body := map[string]any{
		"apiVersion": "relay.wyolet.dev/v1",
		"kind":       "Route",
		"metadata":   map[string]string{"name": "test-route"},
		"spec":       map[string]any{"models": []string{"route-model"}},
	}

	resp = adminReq(t, srv, http.MethodPost, "/control/routes", body)
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("create route: want 201 got %d", resp.StatusCode)
	}
	resp.Body.Close()

	_, ok := store.RouteByName("test-route")
	if !ok {
		t.Fatal("auto-reload: route not in snapshot")
	}

	resp = adminReq(t, srv, http.MethodDelete, "/control/routes/test-route", nil)
	if resp.StatusCode != http.StatusNoContent {
		resp.Body.Close()
		t.Fatalf("delete route: want 204 got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = adminReq(t, srv, http.MethodGet, "/control/routes/test-route", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: want 404 got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- RateLimit ---

func TestAdminRateLimit_CRUD(t *testing.T) {
	srv, store := buildAdminTestServer(t)

	body := map[string]any{
		"apiVersion": "relay.wyolet.dev/v1",
		"kind":       "RateLimit",
		"metadata":   map[string]string{"name": "test-rl"},
		"spec": map[string]any{
			"strategy": "sliding-window",
			"window":   60000000000, // 60s in nanoseconds
			"amount":   100,
		},
	}

	resp := adminReq(t, srv, http.MethodPost, "/control/ratelimits", body)
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("create ratelimit: want 201 got %d", resp.StatusCode)
	}
	resp.Body.Close()

	_, ok := store.RateLimitByName("test-rl")
	if !ok {
		t.Fatal("auto-reload: ratelimit not in snapshot")
	}

	resp = adminReq(t, srv, http.MethodDelete, "/control/ratelimits/test-rl", nil)
	if resp.StatusCode != http.StatusNoContent {
		resp.Body.Close()
		t.Fatalf("delete ratelimit: want 204 got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = adminReq(t, srv, http.MethodGet, "/control/ratelimits/test-rl", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: want 404 got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- Auth ---

func TestAdminCRUD_Auth(t *testing.T) {
	srv, _ := buildAdminTestServer(t)

	endpoints := []struct{ method, path string }{
		{http.MethodGet, "/control/providers"},
		{http.MethodGet, "/control/providers/x"},
		{http.MethodPost, "/control/providers"},
		{http.MethodPut, "/control/providers/x"},
		{http.MethodDelete, "/control/providers/x"},
	}

	for _, ep := range endpoints {
		ep := ep
		t.Run(fmt.Sprintf("%s %s", ep.method, ep.path), func(t *testing.T) {
			// No token → 404
			req, _ := http.NewRequest(ep.method, srv.URL+ep.path, nil)
			resp, _ := http.DefaultClient.Do(req)
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("no token: want 404 got %d", resp.StatusCode)
			}

			// Wrong token → 404
			req, _ = http.NewRequest(ep.method, srv.URL+ep.path, nil)
			req.Header.Set("X-Relay-Admin-Token", "wrong")
			resp, _ = http.DefaultClient.Do(req)
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("wrong token: want 404 got %d", resp.StatusCode)
			}
		})
	}
}

// --- Broken-ref Insert ---

func TestAdminPool_BrokenSecretRef_400(t *testing.T) {
	srv, _ := buildAdminTestServer(t)

	// First insert a provider so policy.provider ref resolves
	provBody := map[string]any{
		"apiVersion": "relay.wyolet.dev/v1", "kind": "Provider",
		"metadata": map[string]string{"name": "ref-prov"},
		"spec":     map[string]any{"kind": "ollama", "baseURL": "http://localhost:11434"},
	}
	resp := adminReq(t, srv, http.MethodPost, "/control/providers", provBody)
	resp.Body.Close()

	// Policy referencing a non-existent secret → 400 (validator catches dangling secret ref)
	body := map[string]any{
		"apiVersion": "relay.wyolet.dev/v1",
		"kind":       "Policy",
		"metadata":   map[string]string{"name": "bad-policy"},
		"spec":       map[string]any{"provider": "ref-prov", "secrets": []string{"nonexistent-secret"}},
	}
	resp = adminReq(t, srv, http.MethodPost, "/control/policies", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("broken ref: want 400 got %d", resp.StatusCode)
	}
	var errOut map[string]map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&errOut); err == nil {
		if errOut["error"]["type"] == "" {
			t.Error("want error envelope")
		}
	}
}
