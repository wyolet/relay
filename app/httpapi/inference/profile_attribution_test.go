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

// metadataAfterDispatch drives one request through the profile-prefixed
// route and returns the lifecycle Metadata the post-flight hook saw.
func metadataAfterDispatch(t *testing.T, mutate func(*http.Request)) map[string]any {
	t.Helper()

	cat, _ := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	d := buildDeps(t, cat)

	profiles := clientprofile.New()
	if err := profiles.Register(stubProfile{}); err != nil {
		t.Fatalf("register profile: %v", err)
	}
	d.Profiles = profiles

	var got map[string]any
	var wg sync.WaitGroup
	wg.Add(1)
	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "test", Fn: func(lc *lifecycle.Context, _ *lifecycle.PostFlightEvent) (any, error) {
		got = lc.Metadata
		wg.Done()
		return nil, nil
	}})
	d.Lifecycle = reg

	r, _ := mountPaths(t, d)

	req := httptest.NewRequest(http.MethodPost, "/stub/v1/messages", strings.NewReader(`{"model":"test-model","max_tokens":16}`))
	req.Header.Set("Content-Type", "application/json")
	mutate(req)
	r.ServeHTTP(httptest.NewRecorder(), req)
	wg.Wait()
	return got
}

func TestDispatch_RecordsProfileAttributionHeaders(t *testing.T) {
	md := metadataAfterDispatch(t, func(r *http.Request) {
		r.Header.Set("x-stub-session-id", "sess-42")
	})
	if got, _ := md["x-stub-session-id"].(string); got != "sess-42" {
		t.Fatalf("metadata[x-stub-session-id] = %v, want sess-42", md["x-stub-session-id"])
	}
}

func TestDispatch_AttributionAbsentHeaderRecordsNothing(t *testing.T) {
	md := metadataAfterDispatch(t, func(*http.Request) {})
	if _, ok := md["x-stub-session-id"]; ok {
		t.Fatalf("absent header must record nothing, got %v", md["x-stub-session-id"])
	}
}

func TestDispatch_AttributionValueIsCapped(t *testing.T) {
	long := strings.Repeat("a", MaxAttributionValueBytes+64)
	md := metadataAfterDispatch(t, func(r *http.Request) {
		r.Header.Set("x-stub-session-id", long)
	})
	got, _ := md["x-stub-session-id"].(string)
	if len(got) != MaxAttributionValueBytes {
		t.Fatalf("recorded value len = %d, want %d", len(got), MaxAttributionValueBytes)
	}
}
