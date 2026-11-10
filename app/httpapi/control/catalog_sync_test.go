package control

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
)

func TestCatalogSyncAnswersTheCheck(t *testing.T) {
	d := Deps{
		Authz: authz.AlwaysAllowAuthenticated{},
		CatalogSync: func(context.Context) (appcatalog.CheckResult, error) {
			return appcatalog.CheckResult{Previous: "v0.2.1", Current: "v0.2.2", Changed: true}, nil
		},
	}
	out, err := syncCatalog(rotateCtx(), d)
	if err != nil {
		t.Fatal(err)
	}
	if out.Body.Previous != "v0.2.1" || out.Body.Current != "v0.2.2" || !out.Body.Changed {
		t.Fatalf("body = %+v", out.Body)
	}
}

func TestCatalogSyncIsAuthorized(t *testing.T) {
	d := Deps{Authz: denyAll{}, CatalogSync: func(context.Context) (appcatalog.CheckResult, error) {
		t.Fatal("the check ran for a denied caller")
		return appcatalog.CheckResult{}, nil
	}}
	if got := statusOf(t, errOf(syncCatalog(rotateCtx(), d))); got != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", got)
	}
}

func TestCatalogSyncStatuses(t *testing.T) {
	for name, tc := range map[string]struct {
		sync func(context.Context) (appcatalog.CheckResult, error)
		want int
	}{
		"not following latest": {nil, http.StatusConflict},
		"another pod seeding": {func(context.Context) (appcatalog.CheckResult, error) {
			return appcatalog.CheckResult{}, appcatalog.ErrSeedBusy
		}, http.StatusServiceUnavailable},
		"check failed": {func(context.Context) (appcatalog.CheckResult, error) {
			return appcatalog.CheckResult{}, errors.New("sha256 mismatch")
		}, http.StatusBadGateway},
	} {
		d := Deps{Authz: authz.AlwaysAllowAuthenticated{}, CatalogSync: tc.sync}
		if got := statusOf(t, errOf(syncCatalog(rotateCtx(), d))); got != tc.want {
			t.Errorf("%s: status = %d, want %d", name, got, tc.want)
		}
	}
}

// The route is mounted under the control API and refuses an anonymous caller.
func TestCatalogSyncRoute(t *testing.T) {
	r := chi.NewRouter()
	Mount(r, mountDeps(t))
	for auth, want := range map[string]int{
		"":                        http.StatusUnauthorized,
		"Bearer test-admin-token": http.StatusConflict,
	} {
		req := httptest.NewRequest(http.MethodPost, "/catalog/sync", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("POST /catalog/sync with %q = %d, want %d: %s", auth, rec.Code, want, rec.Body)
		}
	}
}

func errOf[T any](_ T, err error) error { return err }
