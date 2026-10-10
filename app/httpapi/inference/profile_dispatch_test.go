package inference

// What a request on a client-profile prefix carries into the pipeline: the
// profile's attribution headers, the model name after the profile's rewrite,
// and the same proxy-mode classification as the shape's own path.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/pkg/clientprofile"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/lifecycle"
)

// stubNamedProfile mints picker ids with a "stub/" prefix, the seam the
// claude-code profile uses to satisfy its client's id filter.
type stubNamedProfile struct{ stubProfile }

func (stubNamedProfile) Inbound(model string) string { return strings.TrimPrefix(model, "stub/") }

// dispatchedLifecycle is what the post-flight hook saw of one request.
type dispatchedLifecycle struct {
	metadata       map[string]any
	requestedModel string // stamped after the profile rewrite, read by routing
}

// dispatchWithProfile drives one request for model through path with profile
// registered and returns the lifecycle the post-flight hook saw.
func dispatchWithProfile(t *testing.T, profile clientprofile.Profile, path, model string, mutate func(*http.Request)) dispatchedLifecycle {
	t.Helper()

	cat, _ := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	d := buildDeps(t, cat)

	profiles := clientprofile.New()
	if err := profiles.Register(profile); err != nil {
		t.Fatalf("register profile: %v", err)
	}
	d.Profiles = profiles

	var got dispatchedLifecycle
	var wg sync.WaitGroup
	wg.Add(1)
	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "test", Fn: func(lc *lifecycle.Context, _ *lifecycle.PostFlightEvent) (any, error) {
		got = dispatchedLifecycle{metadata: lc.Metadata, requestedModel: lc.RequestedModel}
		wg.Done()
		return nil, nil
	}})
	d.Lifecycle = reg

	r, _ := mountPaths(t, d)

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"`+model+`","max_tokens":16}`))
	req.Header.Set("Content-Type", "application/json")
	if mutate != nil {
		mutate(req)
	}
	r.ServeHTTP(httptest.NewRecorder(), req)
	wg.Wait()
	return got
}

func TestDispatch_RecordsProfileAttributionHeaders(t *testing.T) {
	md := dispatchWithProfile(t, stubProfile{}, "/stub/v1/messages", "test-model", func(r *http.Request) {
		r.Header.Set("x-stub-session-id", "sess-42")
	}).metadata
	if got, _ := md["x-stub-session-id"].(string); got != "sess-42" {
		t.Fatalf("metadata[x-stub-session-id] = %v, want sess-42", md["x-stub-session-id"])
	}
}

func TestDispatch_AttributionAbsentHeaderRecordsNothing(t *testing.T) {
	md := dispatchWithProfile(t, stubProfile{}, "/stub/v1/messages", "test-model", nil).metadata
	if _, ok := md["x-stub-session-id"]; ok {
		t.Fatalf("absent header must record nothing, got %v", md["x-stub-session-id"])
	}
}

func TestDispatch_AttributionValueIsCapped(t *testing.T) {
	long := strings.Repeat("a", MaxAttributionValueBytes+64)
	md := dispatchWithProfile(t, stubProfile{}, "/stub/v1/messages", "test-model", func(r *http.Request) {
		r.Header.Set("x-stub-session-id", long)
	}).metadata
	got, _ := md["x-stub-session-id"].(string)
	if len(got) != MaxAttributionValueBytes {
		t.Fatalf("recorded value len = %d, want %d", len(got), MaxAttributionValueBytes)
	}
}

func TestDispatch_ProfileStripsMintedModelPrefix(t *testing.T) {
	if got := dispatchWithProfile(t, stubNamedProfile{}, "/stub/v1/messages", "stub/x", nil).requestedModel; got != "x" {
		t.Fatalf("requested model = %q, want x", got)
	}
}

func TestDispatch_MintedPrefixSurvivesWithoutTheProfile(t *testing.T) {
	if got := dispatchWithProfile(t, stubNamedProfile{}, "/anthropic/v1/messages", "stub/x", nil).requestedModel; got != "stub/x" {
		t.Fatalf("requested model = %q, want stub/x — no profile resolved, no rewrite", got)
	}
}

// TestMountRegistry_ProfilePrefixClassifiesProxyMode pins that the mirrored
// profile route is classified from the headers alone: a proxy-mode request
// on the profile prefix takes the same branch, with the same runner source
// and the same rejection, as one on the shape's own path.
func TestMountRegistry_ProfilePrefixClassifiesProxyMode(t *testing.T) {
	cat, _ := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	d := buildDeps(t, cat)

	profiles := clientprofile.New()
	if err := profiles.Register(stubProfile{}); err != nil {
		t.Fatalf("register profile: %v", err)
	}
	d.Profiles = profiles

	sources := make(chan string, 2)
	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "test", Fn: func(lc *lifecycle.Context, _ *lifecycle.PostFlightEvent) (any, error) {
		sources <- lc.Source
		return nil, nil
	}})
	d.Lifecycle = reg

	router := chi.NewRouter()
	router.Use(ClassifyMiddleware())
	api := humachi.New(router, huma.DefaultConfig("test", "1"))
	MountRegistry(buildPathRegistry())(api, d, nil)

	statuses := map[string]int{}
	for _, path := range []string{"/anthropic/v1/messages", "/stub/v1/messages"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"test-model","max_tokens":16}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(httpheader.HeaderProxyMode, httpheader.ProxyModeValueProxy)
		req.Header.Set(httpheader.HeaderRelayAPIKey, "relay-key")
		req.Header.Set("Authorization", "Bearer upstream-key")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		statuses[path] = rec.Code
		if got := <-sources; got != "proxy" {
			t.Fatalf("%s: lifecycle source = %q, want proxy", path, got)
		}
	}
	if statuses["/stub/v1/messages"] != statuses["/anthropic/v1/messages"] {
		t.Fatalf("status on the profile prefix = %d, want the shape path's %d",
			statuses["/stub/v1/messages"], statuses["/anthropic/v1/messages"])
	}
}
