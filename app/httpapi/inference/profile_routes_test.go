package inference

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/pkg/clientprofile"
)

// stubRoutedProfile adds the optional Router and ModelLister seams on top
// of stubProfile, so a full Mount exercises both.
type stubRoutedProfile struct{ stubProfile }

func (stubRoutedProfile) Routes() []clientprofile.Route {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return []clientprofile.Route{
		{Method: http.MethodHead, Path: "/api/hello", Handler: ok, Public: true},
		{Method: http.MethodGet, Path: "/api/hello", Handler: ok, Public: true},
		{Method: http.MethodGet, Path: "/api/private", Handler: ok},
	}
}

func (stubRoutedProfile) Models([]clientprofile.ModelEntry) ([]byte, string, error) {
	return []byte(`{"data":[]}`), "application/json", nil
}

func mountWithRoutedProfile(t *testing.T) chi.Router {
	t.Helper()
	cat, _ := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	d := buildDeps(t, cat)
	d.Pinger = stubPinger{}

	profiles := clientprofile.New()
	if err := profiles.Register(stubRoutedProfile{}); err != nil {
		t.Fatalf("register profile: %v", err)
	}
	d.Profiles = profiles

	r := chi.NewRouter()
	Mount(r, d)
	return r
}

func TestMount_ProfileHelloIsPublic(t *testing.T) {
	r := mountWithRoutedProfile(t)
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(method, "/stub/api/hello", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s /stub/api/hello = %d, want 200 without credentials", method, rec.Code)
		}
	}
}

func TestMount_ProfileNonPublicRouteNeedsARelayKey(t *testing.T) {
	r := mountWithRoutedProfile(t)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stub/api/private", nil))
	if rec.Code == http.StatusOK {
		t.Fatal("a non-public profile route must run the relay-key auth chain")
	}
}

func TestMount_ProfileModelsRouteRegistered(t *testing.T) {
	r := mountWithRoutedProfile(t)
	var found bool
	err := chi.Walk(r, func(method string, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet && route == "/stub/v1/models" {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk routes: %v", err)
	}
	if !found {
		t.Fatal("GET /stub/v1/models not registered for a ModelLister profile")
	}
}

// OpenCode reads its catalog as a models.dev document rather than a
// list-models response, so its profile declares its own path — and the
// document has to name the endpoint the reader should call back on.
func TestProfileModels_OpenCodeCatalogPathAndAPIURL(t *testing.T) {
	cat, rk := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	for _, m := range cat.Current().AllModels() {
		m.Spec.ContextWindowTotal = 128000
	}
	d := buildDeps(t, cat)
	profiles := clientprofile.New()
	if err := profiles.Register(clientprofile.OpenCode()); err != nil {
		t.Fatalf("register profile: %v", err)
	}
	d.Profiles = profiles

	get := func(t *testing.T, d Deps) map[string]struct {
		API    string                     `json:"api"`
		Models map[string]json.RawMessage `json:"models"`
	} {
		t.Helper()
		r := chi.NewRouter()
		api := humachi.New(r, huma.DefaultConfig("test", "1"))
		registerProfileModels(api, d, nil)

		rec := httptest.NewRecorder()
		req := withNormalContext(httptest.NewRequest(http.MethodGet, "http://relay.internal:8080/opencode/api.json", nil), rk)
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /opencode/api.json = %d — body: %s", rec.Code, rec.Body.String())
		}
		var doc map[string]struct {
			API    string                     `json:"api"`
			Models map[string]json.RawMessage `json:"models"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("catalog document: %v — body: %s", err, rec.Body.String())
		}
		return doc
	}

	// No configured public URL: the document names the origin the request
	// arrived on.
	if got := get(t, d)["relay"]; got.API != "http://relay.internal:8080/opencode/v1" {
		t.Errorf("api = %q, want the request's own origin", got.API)
	} else if len(got.Models) == 0 {
		t.Error("granted models must appear in the catalog document")
	}

	// Configured: that wins, since the caller may have reached an internal
	// address the client cannot use.
	d.PublicURL = "https://relay.example.com/"
	if got := get(t, d)["relay"].API; got != "https://relay.example.com/opencode/v1" {
		t.Errorf("api = %q, want the configured public URL", got)
	}
}

// The Codex CLI resolves the list endpoint against its base URL as
// {base_url}/models?client_version=<v>, so with base_url at the profile
// prefix the path is /codex/v1/models and the query is one the endpoint
// never declared: it must be ignored, not rejected.
func TestProfileModels_CodexPathAndUnknownQuery(t *testing.T) {
	cat, rk := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	d := buildDeps(t, cat)
	profiles := clientprofile.New()
	if err := profiles.Register(clientprofile.Codex()); err != nil {
		t.Fatalf("register profile: %v", err)
	}
	d.Profiles = profiles

	r := chi.NewRouter()
	api := humachi.New(r, huma.DefaultConfig("test", "1"))
	registerProfileModels(api, d, nil)

	rec := httptest.NewRecorder()
	req := withNormalContext(httptest.NewRequest(http.MethodGet, "/codex/v1/models?client_version=0.153.4", nil), rk)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /codex/v1/models?client_version= = %d, want 200 — body: %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("catalog document: %v — body: %s", err, rec.Body.String())
	}
	if len(doc.Models) == 0 {
		t.Error("granted models must appear in the catalog document")
	}
}
