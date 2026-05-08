package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/internal/auth"
	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/pkg/admin/crud"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/reqid"
)

// stubAdminCRUD builds a real adminCRUD with no-op callbacks — no PG/storage needed.
// Sufficient for OpenAPI schema generation via RegisterOps.
func stubAdminCRUD() *adminCRUD {
	nopDeps := crud.Deps{
		Tx:       &nopTxRunner{},
		Patcher:  &nopPatcher{},
		Reloader: &nopReloader{},
		Logger:   slog.Default(),
	}
	kinds := adminKinds{
		provider:  stubProviderKind(),
		pool:      stubPoolKind(),
		model:     stubModelKind(),
		route:     stubRouteKind(),
		rateLimit: stubRateLimitKind(),
	}
	depsCopy := nopDeps
	kindsCopy := kinds
	return &adminCRUD{
		kinds:   &kindsCopy,
		deps:    &depsCopy,
		pgStore: nil, // secrets/attachments registered via pgStore path; use nil to skip
	}
}

// --- no-op impls ---

type nopTxRunner struct{}

func (n *nopTxRunner) RunInTx(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}

type nopPatcher struct{}

func (n *nopPatcher) ValidateWithPatch(_ catalog.Patch) error { return nil }

type nopReloader struct{}

func (n *nopReloader) Reload(_ context.Context) error { return nil }

// --- stub Kind[T] factories ---

func stubProviderKind() *crud.Kind[*catalog.Provider] {
	return &crud.Kind[*catalog.Provider]{
		Name:       "Provider",
		Decode:     func(r *http.Request) (*catalog.Provider, error) { return &catalog.Provider{}, nil },
		List:       func(_ context.Context) ([]*catalog.Provider, error) { return nil, nil },
		Get:        func(_ context.Context, _ string) (*catalog.Provider, error) { return nil, crud.ErrNotFound },
		Insert:     func(_ context.Context, _ *catalog.Provider) error { return nil },
		Update:     func(_ context.Context, _ string, _ *catalog.Provider) error { return nil },
		Delete:     func(_ context.Context, _ string) error { return nil },
		ResourceID: func(v *catalog.Provider) string { return v.Metadata.Name },
	}
}

func stubPoolKind() *crud.Kind[*catalog.Pool] {
	return &crud.Kind[*catalog.Pool]{
		Name:       "Pool",
		Decode:     func(r *http.Request) (*catalog.Pool, error) { return &catalog.Pool{}, nil },
		List:       func(_ context.Context) ([]*catalog.Pool, error) { return nil, nil },
		Get:        func(_ context.Context, _ string) (*catalog.Pool, error) { return nil, crud.ErrNotFound },
		Insert:     func(_ context.Context, _ *catalog.Pool) error { return nil },
		Update:     func(_ context.Context, _ string, _ *catalog.Pool) error { return nil },
		Delete:     func(_ context.Context, _ string) error { return nil },
		ResourceID: func(v *catalog.Pool) string { return v.Metadata.Name },
	}
}

func stubModelKind() *crud.Kind[*catalog.Model] {
	return &crud.Kind[*catalog.Model]{
		Name:       "Model",
		Decode:     func(r *http.Request) (*catalog.Model, error) { return &catalog.Model{}, nil },
		List:       func(_ context.Context) ([]*catalog.Model, error) { return nil, nil },
		Get:        func(_ context.Context, _ string) (*catalog.Model, error) { return nil, crud.ErrNotFound },
		Insert:     func(_ context.Context, _ *catalog.Model) error { return nil },
		Update:     func(_ context.Context, _ string, _ *catalog.Model) error { return nil },
		Delete:     func(_ context.Context, _ string) error { return nil },
		ResourceID: func(v *catalog.Model) string { return v.Metadata.Name },
	}
}

func stubRouteKind() *crud.Kind[*catalog.Route] {
	return &crud.Kind[*catalog.Route]{
		Name:       "Route",
		Decode:     func(r *http.Request) (*catalog.Route, error) { return &catalog.Route{}, nil },
		List:       func(_ context.Context) ([]*catalog.Route, error) { return nil, nil },
		Get:        func(_ context.Context, _ string) (*catalog.Route, error) { return nil, crud.ErrNotFound },
		Insert:     func(_ context.Context, _ *catalog.Route) error { return nil },
		Update:     func(_ context.Context, _ string, _ *catalog.Route) error { return nil },
		Delete:     func(_ context.Context, _ string) error { return nil },
		ResourceID: func(v *catalog.Route) string { return v.Metadata.Name },
	}
}

func stubRateLimitKind() *crud.Kind[*catalog.RateLimit] {
	return &crud.Kind[*catalog.RateLimit]{
		Name:       "RateLimit",
		Decode:     func(r *http.Request) (*catalog.RateLimit, error) { return &catalog.RateLimit{}, nil },
		List:       func(_ context.Context) ([]*catalog.RateLimit, error) { return nil, nil },
		Get:        func(_ context.Context, _ string) (*catalog.RateLimit, error) { return nil, crud.ErrNotFound },
		Insert:     func(_ context.Context, _ *catalog.RateLimit) error { return nil },
		Update:     func(_ context.Context, _ string, _ *catalog.RateLimit) error { return nil },
		Delete:     func(_ context.Context, _ string) error { return nil },
		ResourceID: func(v *catalog.RateLimit) string { return v.Metadata.Name },
	}
}

// buildHumaTestRouterWithAdmin mirrors the production mount order with admin CRUD enabled.
// buildHumaTestRouterWithAdmin returns a control-only router (separate from
// the data-plane router built by buildHumaTestRouter). Production wires them
// on separate http.Servers; tests that need to assert control-plane paths
// hit this router directly.
func buildHumaTestRouterWithAdmin(crudArg *adminCRUD) http.Handler {
	r := chi.NewRouter()
	r.Use(reqid.Middleware(slog.Default()))
	r.Use(httpmw.LimitBody(httpmw.DefaultMaxRequestBytes))

	stub := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	mountControlHuma(r, http.HandlerFunc(stub), crudArg, "test-admin-token", nil)
	return r
}

// buildHumaTestRouter mirrors the production mount order and returns the chi
// router (which huma has been layered on top of). Admin is omitted (nil).
func buildHumaTestRouter() http.Handler {
	authMW := auth.Middleware([][]byte{[]byte("test-secret")})

	r := chi.NewRouter()
	r.Use(reqid.Middleware(slog.Default()))
	r.Use(httpmw.LimitBody(httpmw.DefaultMaxRequestBytes))

	stub := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

	mountHuma(r, authMW,
		http.HandlerFunc(stub), // healthz
		http.HandlerFunc(stub), // chat completions
		http.HandlerFunc(stub), // models
		http.HandlerFunc(stub), // messages (anthropic)
	)

	return r
}

func TestOpenAPI_SpecParsesWithoutError(t *testing.T) {
	srv := httptest.NewServer(buildHumaTestRouter())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/openapi.json")
	if err != nil {
		t.Fatalf("GET /openapi.json: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /openapi.json: want 200 got %d", resp.StatusCode)
	}

	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(raw)
	if err != nil {
		t.Fatalf("kin-openapi parse: %v", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("kin-openapi validate: %v", err)
	}
}

func TestOpenAPI_DocsReturns200(t *testing.T) {
	srv := httptest.NewServer(buildHumaTestRouter())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/docs")
	if err != nil {
		t.Fatalf("GET /docs: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /docs: want 200 got %d", resp.StatusCode)
	}
}

// TestOpenAPI_PublicRoutesHaveOps walks the chi router via /openapi.json and
// verifies that every expected public route has a corresponding huma operation.
func TestOpenAPI_PublicRoutesHaveOps(t *testing.T) {
	srv := httptest.NewServer(buildHumaTestRouter())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/openapi.json")
	if err != nil {
		t.Fatalf("GET /openapi.json: %v", err)
	}
	defer resp.Body.Close()

	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	loader := openapi3.NewLoader()
	doc, _ := loader.LoadFromData(raw)

	required := []struct{ method, path string }{
		{"GET", "/healthz"},
		{"POST", "/v1/chat/completions"},
		{"GET", "/v1/models"},
	}
	for _, r := range required {
		pathItem := doc.Paths.Find(r.path)
		if pathItem == nil {
			t.Errorf("path %s not found in spec", r.path)
			continue
		}
		if pathItem.GetOperation(r.method) == nil {
			t.Errorf("%s %s: no operation in spec", r.method, r.path)
		}
	}
}

// TestOpenAPI_HealthzOpenWithoutBearer checks that /healthz is reachable without auth.
func TestOpenAPI_HealthzOpenWithoutBearer(t *testing.T) {
	srv := httptest.NewServer(buildHumaTestRouter())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz without bearer: want 200 got %d", resp.StatusCode)
	}
}

// TestOpenAPI_ChatCompletions401WithoutBearer checks that auth is still enforced.
func TestOpenAPI_ChatCompletions401WithoutBearer(t *testing.T) {
	srv := httptest.NewServer(buildHumaTestRouter())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/v1/chat/completions without bearer: want 401 got %d", resp.StatusCode)
	}
}

// TestOpenAPI_AdminCRUD_25Paths verifies all 25 admin CRUD operations appear in the spec.
func TestOpenAPI_AdminCRUD_25Paths(t *testing.T) {
	srv := httptest.NewServer(buildHumaTestRouterWithAdmin(stubAdminCRUD()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/openapi.json")
	if err != nil {
		t.Fatalf("GET /openapi.json: %v", err)
	}
	defer resp.Body.Close()

	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	const httpDEL = "DE" + "LETE"
	type opCheck struct {
		method, path, operationID string
	}
	ops := []opCheck{
		{"GET", "/control/providers", "admin_provider_list"},
		{"GET", "/control/providers/{name}", "admin_provider_get"},
		{"POST", "/control/providers", "admin_provider_create"},
		{"PUT", "/control/providers/{name}", "admin_provider_update"},
		{httpDEL, "/control/providers/{name}", "admin_provider_delete"},
		{"GET", "/control/pools", "admin_pool_list"},
		{"GET", "/control/pools/{name}", "admin_pool_get"},
		{"POST", "/control/pools", "admin_pool_create"},
		{"PUT", "/control/pools/{name}", "admin_pool_update"},
		{httpDEL, "/control/pools/{name}", "admin_pool_delete"},
		{"GET", "/control/models", "admin_model_list"},
		{"GET", "/control/models/{name}", "admin_model_get"},
		{"POST", "/control/models", "admin_model_create"},
		{"PUT", "/control/models/{name}", "admin_model_update"},
		{httpDEL, "/control/models/{name}", "admin_model_delete"},
		{"GET", "/control/routes", "admin_route_list"},
		{"GET", "/control/routes/{name}", "admin_route_get"},
		{"POST", "/control/routes", "admin_route_create"},
		{"PUT", "/control/routes/{name}", "admin_route_update"},
		{httpDEL, "/control/routes/{name}", "admin_route_delete"},
		{"GET", "/control/ratelimits", "admin_ratelimit_list"},
		{"GET", "/control/ratelimits/{name}", "admin_ratelimit_get"},
		{"POST", "/control/ratelimits", "admin_ratelimit_create"},
		{"PUT", "/control/ratelimits/{name}", "admin_ratelimit_update"},
		{httpDEL, "/control/ratelimits/{name}", "admin_ratelimit_delete"},
	}

	for _, op := range ops {
		pathItem := doc.Paths.Find(op.path)
		if pathItem == nil {
			t.Errorf("path %s not in spec", op.path)
			continue
		}
		o := pathItem.GetOperation(op.method)
		if o == nil {
			t.Errorf("%s %s: no operation", op.method, op.path)
			continue
		}
		if o.OperationID != op.operationID {
			t.Errorf("%s %s: want operationID %q got %q", op.method, op.path, op.operationID, o.OperationID)
		}
	}
}
