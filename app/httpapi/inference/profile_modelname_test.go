package inference

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/pkg/clientprofile"
	"github.com/wyolet/relay/pkg/lifecycle"
)

// stubNamedProfile mints picker ids with a "stub/" prefix, the seam the
// claude-code profile uses to satisfy its client's id filter.
type stubNamedProfile struct{ stubProfile }

func (stubNamedProfile) Inbound(model string) string { return strings.TrimPrefix(model, "stub/") }

// requestedModelAfterDispatch drives one request through the given path and
// returns the model name the lifecycle Context carries — stamped after the
// profile rewrite and read by routing.
func requestedModelAfterDispatch(t *testing.T, path, model string) string {
	t.Helper()

	cat, _ := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	d := buildDeps(t, cat)

	profiles := clientprofile.New()
	if err := profiles.Register(stubNamedProfile{}); err != nil {
		t.Fatalf("register profile: %v", err)
	}
	d.Profiles = profiles

	var got string
	var wg sync.WaitGroup
	wg.Add(1)
	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "test", Fn: func(lc *lifecycle.Context, _ *lifecycle.PostFlightEvent) (any, error) {
		got = lc.RequestedModel
		wg.Done()
		return nil, nil
	}})
	d.Lifecycle = reg

	r, _ := mountPaths(t, d)

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"`+model+`","max_tokens":16}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(httptest.NewRecorder(), req)
	wg.Wait()
	return got
}

func TestDispatch_ProfileStripsMintedModelPrefix(t *testing.T) {
	if got := requestedModelAfterDispatch(t, "/stub/v1/messages", "stub/x"); got != "x" {
		t.Fatalf("requested model = %q, want x", got)
	}
}

func TestDispatch_MintedPrefixSurvivesWithoutTheProfile(t *testing.T) {
	if got := requestedModelAfterDispatch(t, "/anthropic/v1/messages", "stub/x"); got != "stub/x" {
		t.Fatalf("requested model = %q, want stub/x — no profile resolved, no rewrite", got)
	}
}
