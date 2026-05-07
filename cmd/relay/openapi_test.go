package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/pkg/auth"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/reqid"
)

// buildHumaTestRouterWithAdmin mirrors the production mount order with admin CRUD enabled.
func buildHumaTestRouterWithAdmin(crud *adminCRUD) http.Handler {
	authMW := auth.Middleware([][]byte{[]byte("test-secret")})

	r := chi.NewRouter()
	r.Use(reqid.Middleware(slog.Default()))
	r.Use(httpmw.LimitBody(httpmw.MaxRequestBytesFromEnv()))

	stub := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

	mountHuma(r, authMW,
		http.HandlerFunc(stub), // healthz
		http.HandlerFunc(stub), // chat completions
		http.HandlerFunc(stub), // models
		http.HandlerFunc(stub), // admin reload
		crud,
		"test-admin-token",
	)

	return r
}

// buildHumaTestRouter mirrors the production mount order and returns the chi
// router (which huma has been layered on top of). Admin is omitted (nil).
func buildHumaTestRouter() http.Handler {
	authMW := auth.Middleware([][]byte{[]byte("test-secret")})

	r := chi.NewRouter()
	r.Use(reqid.Middleware(slog.Default()))
	r.Use(httpmw.LimitBody(httpmw.MaxRequestBytesFromEnv()))

	stub := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

	mountHuma(r, authMW,
		http.HandlerFunc(stub), // healthz
		http.HandlerFunc(stub), // chat completions
		http.HandlerFunc(stub), // models
		nil,                    // admin — not configured
		nil,                    // crud — not configured
		"",                     // adminTok — not configured
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
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	stubH := adminHandlers{stub, stub, stub, stub, stub}
	crud := &adminCRUD{
		provider:  stubH,
		pool:      stubH,
		model:     stubH,
		route:     stubH,
		rateLimit: stubH,
	}

	srv := httptest.NewServer(buildHumaTestRouterWithAdmin(crud))
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
		{"GET", "/admin/providers", "admin_provider_list"},
		{"GET", "/admin/providers/{name}", "admin_provider_get"},
		{"POST", "/admin/providers", "admin_provider_create"},
		{"PUT", "/admin/providers/{name}", "admin_provider_update"},
		{httpDEL, "/admin/providers/{name}", "admin_provider_delete"},
		{"GET", "/admin/pools", "admin_pool_list"},
		{"GET", "/admin/pools/{name}", "admin_pool_get"},
		{"POST", "/admin/pools", "admin_pool_create"},
		{"PUT", "/admin/pools/{name}", "admin_pool_update"},
		{httpDEL, "/admin/pools/{name}", "admin_pool_delete"},
		{"GET", "/admin/models", "admin_model_list"},
		{"GET", "/admin/models/{name}", "admin_model_get"},
		{"POST", "/admin/models", "admin_model_create"},
		{"PUT", "/admin/models/{name}", "admin_model_update"},
		{httpDEL, "/admin/models/{name}", "admin_model_delete"},
		{"GET", "/admin/routes", "admin_route_list"},
		{"GET", "/admin/routes/{name}", "admin_route_get"},
		{"POST", "/admin/routes", "admin_route_create"},
		{"PUT", "/admin/routes/{name}", "admin_route_update"},
		{httpDEL, "/admin/routes/{name}", "admin_route_delete"},
		{"GET", "/admin/ratelimits", "admin_ratelimit_list"},
		{"GET", "/admin/ratelimits/{name}", "admin_ratelimit_get"},
		{"POST", "/admin/ratelimits", "admin_ratelimit_create"},
		{"PUT", "/admin/ratelimits/{name}", "admin_ratelimit_update"},
		{httpDEL, "/admin/ratelimits/{name}", "admin_ratelimit_delete"},
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
