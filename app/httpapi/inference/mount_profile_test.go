package inference

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/pkg/clientprofile"
	"github.com/wyolet/relay/pkg/lifecycle"
)

// stubProfile speaks the anthropic shape and never matches on User-Agent,
// so tests exercise the prefix mount alone.
type stubProfile struct{}

func (stubProfile) Name() string             { return "stub" }
func (stubProfile) Shape() string            { return string(adapters.Anthropic) }
func (stubProfile) Match(*http.Request) bool { return false }

func (stubProfile) AttributionHeaders() []string { return []string{"x-stub-session-id"} }

// buildPathRegistry is buildTestRegistry's counterpart for mount tests: the
// anthropic spec carries the inbound path the profile mirrors.
func buildPathRegistry() *adapter.Registry {
	anthropicSpec := (&adapter.Spec{
		Name: adapters.Anthropic,
		InboundPaths: []adapter.InboundPath{
			{Path: "/anthropic/v1/messages", OperationID: "anthropic_messages", Summary: "Create a message"},
		},
		DefaultPath: "/v1/messages",
		Auth:        adapter.AuthStrategy{Header: "x-api-key"},
		Translator:  stubV1Translator{},
	}).Build()
	return adapter.NewRegistry(anthropicSpec)
}

// mountPaths mounts the path-carrying registry with the given profiles and
// returns the chi routes and the huma API for inspection.
func mountPaths(t *testing.T, d Deps) (chi.Router, huma.API) {
	t.Helper()
	r := chi.NewRouter()
	api := humachi.New(r, huma.DefaultConfig("test", "1"))
	MountRegistry(buildPathRegistry())(api, d, nil)
	return r, api
}

func routePaths(t *testing.T, r chi.Router) []string {
	t.Helper()
	var paths []string
	err := chi.Walk(r, func(_ string, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		paths = append(paths, route)
		return nil
	})
	if err != nil {
		t.Fatalf("walk routes: %v", err)
	}
	sort.Strings(paths)
	return paths
}

func contains(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

// TestMountRegistry_ProfilePrefixReachesSameHandler asserts the mirrored
// route hits the same shape handler and that the profile rides the request
// context all the way to the lifecycle Context.
func TestMountRegistry_ProfilePrefixReachesSameHandler(t *testing.T) {
	cat, _ := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	d := buildDeps(t, cat)

	profiles := clientprofile.New()
	if err := profiles.Register(stubProfile{}); err != nil {
		t.Fatalf("register profile: %v", err)
	}
	d.Profiles = profiles

	var gotClient string
	var wg sync.WaitGroup
	wg.Add(1)
	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "test", Fn: func(lc *lifecycle.Context, _ *lifecycle.PostFlightEvent) (any, error) {
		gotClient, _ = lc.Metadata["client"].(string)
		wg.Done()
		return nil, nil
	}})
	d.Lifecycle = reg

	r, _ := mountPaths(t, d)

	body := strings.NewReader(`{"model":"test-model","max_tokens":16}`)
	req := httptest.NewRequest(http.MethodPost, "/stub/v1/messages", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// No classification / relay key on the context, so the shape handler
	// rejects the request — reaching it at all is what the mirrored route
	// proves.
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 from the shape handler; body: %s", rec.Code, rec.Body.String())
	}
	wg.Wait()
	if gotClient != "stub" {
		t.Fatalf("lifecycle metadata client = %q, want stub", gotClient)
	}
}

// TestMountRegistry_NilProfilesLeavesRoutesUnchanged pins the "zero
// behaviour change" contract: without profiles the route set is exactly
// the spec's own inbound paths.
func TestMountRegistry_NilProfilesLeavesRoutesUnchanged(t *testing.T) {
	cat, _ := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)

	base := buildDeps(t, cat)
	baseRouter, baseAPI := mountPaths(t, base)
	basePaths := routePaths(t, baseRouter)

	if !contains(basePaths, "/anthropic/v1/messages") {
		t.Fatalf("base routes = %v, want the spec inbound path", basePaths)
	}
	if _, ok := baseAPI.OpenAPI().Paths["/anthropic/v1/messages"]; !ok {
		t.Fatal("spec inbound path missing from the OpenAPI doc")
	}

	withProfile := buildDeps(t, cat)
	profiles := clientprofile.New()
	if err := profiles.Register(stubProfile{}); err != nil {
		t.Fatalf("register profile: %v", err)
	}
	withProfile.Profiles = profiles
	profRouter, profAPI := mountPaths(t, withProfile)

	// Registering a profile adds exactly one route: the mirrored path.
	var added []string
	for _, p := range routePaths(t, profRouter) {
		if !contains(basePaths, p) {
			added = append(added, p)
		}
	}
	if len(added) != 1 || added[0] != "/stub/v1/messages" {
		t.Fatalf("routes added by the profile = %v, want [/stub/v1/messages]", added)
	}
	// The mirrored route is the same operation, so it stays out of the doc.
	if _, ok := profAPI.OpenAPI().Paths["/stub/v1/messages"]; ok {
		t.Error("mirrored profile route must be hidden from the OpenAPI doc")
	}
}
