package inference

import (
	"net/http"
	"net/http/httptest"
	"testing"

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
