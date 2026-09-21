package inference

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/pkg/clientprofile"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/lifecycle"
)

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
