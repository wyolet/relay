package inference

import (
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/clientprofile"
)

// stubRoutedProfile adds the optional Router and ModelLister seams on top of stubProfile, so a full Mount exercises both. Its list is the ids it was handed, or err.
type stubRoutedProfile struct {
	stubProfile
	err error
}

func (stubRoutedProfile) Routes() []clientprofile.Route {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return []clientprofile.Route{
		{Method: http.MethodHead, Path: "/api/hello", Handler: ok, Public: true},
		{Method: http.MethodGet, Path: "/api/hello", Handler: ok, Public: true},
		{Method: http.MethodGet, Path: "/api/private", Handler: ok},
	}
}

func (p stubRoutedProfile) Models(entries []clientprofile.ModelEntry) ([]byte, string, error) {
	body := "ids:"
	for _, e := range entries {
		body += " " + e.ID
	}
	return []byte(body), "text/plain", p.err
}

// A profile's public routes answer without a credential; its other routes, its model list included, run the relay-key chain of /v1/*. The /a/{account}/ prefix serves under every route Mount registers, with no route of its own.
func TestMount_ProfileRoutes(t *testing.T) {
	cat, _ := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	d := buildDeps(t, cat)
	d.Pinger = stubPinger{}
	d.Profiles = profileRegistry(stubRoutedProfile{})
	r := chi.NewRouter()
	Mount(r, d)

	for _, prefix := range []string{"", "/a/acme-ml"} {
		for _, tc := range []struct {
			method, path string
			status       int
		}{
			{http.MethodHead, "/stub/api/hello", http.StatusOK},
			{http.MethodGet, "/stub/api/hello", http.StatusOK},
			{http.MethodGet, "/healthz", http.StatusOK},
			{http.MethodGet, "/stub/api/private", http.StatusUnauthorized},
			{http.MethodGet, "/stub/v1/models", http.StatusUnauthorized},
			{http.MethodGet, "/v1/models", http.StatusUnauthorized},
			{http.MethodGet, "/stub/api/hidden", http.StatusNotFound},
			{http.MethodPost, "/stub/api/hello", http.StatusMethodNotAllowed},
		} {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(tc.method, prefix+tc.path, nil))
			if rec.Code != tc.status {
				t.Errorf("%s %s = %d, want %d", tc.method, prefix+tc.path, rec.Code, tc.status)
			}
			if tc.status == http.StatusUnauthorized {
				wantUnauthenticated(t, rec, "missing relay key")
			}
		}
	}
}

// Each profile that renders a model list gets it at its own path under its prefix, rendered from what the caller's key reaches.
func TestProfileModels(t *testing.T) {
	const codexEntry = `{"slug":"test-model","display_name":"test-model","description":"anthropic","base_instructions":"","supported_reasoning_levels":[],"shell_type":"unified_exec","visibility":"list","supported_in_api":true,"priority":0,"support_verbosity":false,"truncation_policy":{"mode":"bytes","limit":10000},"experimental_supported_tools":[],"availability_nux":null,"upgrade":null,"default_verbosity":null,"apply_patch_tool_type":null}`
	opencode := func(api string) string {
		return `{"relay":{"id":"relay","name":"Wyolet Relay","env":["RELAY_API_KEY"],"npm":"@ai-sdk/openai-compatible","api":"` + api + `","models":{}}}`
	}
	for _, tc := range []struct {
		name      string
		profile   clientprofile.Profile
		publicURL string
		target    string
		edit      func(*http.Request)
		anonymous bool // the 401's envelope: TestListModels_NoCredentialCarriesTheCode
		status    int
		ctype     string
		body      string
	}{
		{name: "plain lister", profile: stubRoutedProfile{}, target: "/stub/v1/models", status: 200, ctype: "text/plain", body: "ids: test-model"},
		// Codex asks {base_url}/models?client_version=<v>: a query the endpoint never declared is ignored, not rejected.
		{name: "codex, undeclared query", profile: clientprofile.Codex(), target: "/codex/v1/models?client_version=0.153.4", status: 200, ctype: "application/json", body: `{"models":[` + codexEntry + `]}`},
		// OpenCode reads a models.dev document at its own path, which names the endpoint its reader calls back on: the origin the request arrived at unless the deployment declares a public URL.
		{name: "opencode, request origin", profile: clientprofile.OpenCode(), target: "http://relay.internal:8080/opencode/api.json", status: 200, ctype: "application/json",
			body: opencode("http://relay.internal:8080/opencode/v1")},
		{name: "opencode, TLS request", profile: clientprofile.OpenCode(), target: "http://relay.internal/opencode/api.json", edit: func(r *http.Request) { r.TLS = &tls.ConnectionState{} },
			status: 200, ctype: "application/json", body: opencode("https://relay.internal/opencode/v1")},
		{name: "opencode, forwarded scheme", profile: clientprofile.OpenCode(), target: "http://relay.internal/opencode/api.json", edit: func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", " https , http") },
			status: 200, ctype: "application/json", body: opencode("https://relay.internal/opencode/v1")},
		{name: "opencode, declared public URL", profile: clientprofile.OpenCode(), publicURL: "https://relay.example.com/", target: "http://relay.internal:8080/opencode/api.json",
			status: 200, ctype: "application/json", body: opencode("https://relay.example.com/opencode/v1")},
		{name: "render fails", profile: stubRoutedProfile{err: errors.New("no template")}, target: "/stub/v1/models", status: 500, ctype: "application/json",
			body: `{"error":{"type":"api_error","code":"render_models","message":"no template"}}`},
		{name: "no credential", profile: stubRoutedProfile{}, target: "/stub/v1/models", anonymous: true, status: 401},
		{name: "profile without a list", profile: stubProfile{}, target: "/stub/v1/models", status: 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			httpapi.InstallErrorRewriter()
			cat, rk := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
			d := buildDeps(t, cat)
			d.Profiles, d.PublicURL = profileRegistry(tc.profile), tc.publicURL
			r := chi.NewRouter()
			registerProfileModels(humachi.New(r, huma.DefaultConfig("test", "1")), d, nil)

			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			if !tc.anonymous {
				req = withNormalContext(req, rk)
			}
			if tc.edit != nil {
				tc.edit(req)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.status, rec.Body)
			}
			if tc.body == "" {
				return
			}
			if diff := cmp.Diff(tc.body, rec.Body.String()); diff != "" {
				t.Errorf("body (-want +got):\n%s", diff)
			}
			if got := rec.Header().Get("Content-Type"); got != tc.ctype {
				t.Errorf("Content-Type = %q, want %q", got, tc.ctype)
			}
		})
	}
}
