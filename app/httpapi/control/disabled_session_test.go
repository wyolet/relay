package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/catalog/catalogtest"
)

// disabledUserSource is the catalog's users source in these tests: the ids it reports disabled change between reloads.
type disabledUserSource struct {
	mu       sync.Mutex
	disabled map[string]struct{}
}

func (u *disabledUserSource) TokenVersionsAndDisabled(context.Context) (map[string]int, map[string]struct{}, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make(map[string]struct{}, len(u.disabled))
	for id := range u.disabled {
		out[id] = struct{}{}
	}
	return map[string]int{}, out, nil
}

func (u *disabledUserSource) setDisabled(t *testing.T, cat *appcatalog.Catalog, ids ...string) {
	t.Helper()
	u.mu.Lock()
	u.disabled = map[string]struct{}{}
	for _, id := range ids {
		u.disabled[id] = struct{}{}
	}
	u.mu.Unlock()
	if err := cat.ReloadTokenVersions(t.Context()); err != nil {
		t.Fatalf("reload users: %v", err)
	}
}

const disabledBody = `{"error":{"type":"invalid_request_error","message":"account is disabled"}}`

// A session can outlive its account's disable (the session purge failed, or a login raced it). The control plane refuses such a session on every route but logout, from the snapshot alone, and honours it again once the account is re-enabled.
func TestControlRefusesDisabledAccountSession(t *testing.T) {
	for _, prefix := range []string{"", "/api"} {
		t.Run("mounted at "+prefix+"/", func(t *testing.T) {
			rows := &disabledUserSource{}
			cat := catalogtest.Catalog{}.New()
			cat.UseTokenVersions(rows)
			if err := cat.Reload(t.Context()); err != nil {
				t.Fatalf("reload: %v", err)
			}
			h, _ := authHarnessAt(t, prefix, cat)
			sess := loginAtPrefix(t, h, prefix, `{"userId":"u-1"}`)
			// A YAML-file login carries its slug as the id and has no users row.
			yamlSess := loginAtPrefix(t, h, prefix, `{"userId":"alice"}`)

			do := func(method, path, bearer string, c *http.Cookie) *httptest.ResponseRecorder {
				req := httptest.NewRequest(method, prefix+path, nil)
				if bearer != "" {
					req.Header.Set("Authorization", "Bearer "+bearer)
				}
				if c != nil {
					req.AddCookie(c)
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				return rec
			}
			wantStatus := func(rec *httptest.ResponseRecorder, want int, what string) {
				t.Helper()
				if rec.Code != want {
					t.Fatalf("%s = %d, want %d: %s", what, rec.Code, want, rec.Body)
				}
			}
			wantDisabled := func(rec *httptest.ResponseRecorder, what string) {
				t.Helper()
				wantStatus(rec, http.StatusForbidden, what)
				if got := rec.Body.String(); got != disabledBody {
					t.Fatalf("%s body = %s, want %s", what, got, disabledBody)
				}
				if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
					t.Fatalf("%s content-type = %q, want application/json", what, ct)
				}
			}

			rows.setDisabled(t, cat, "u-1")
			wantDisabled(do(http.MethodGet, "/test/caller", "", sess), "protected route")
			wantDisabled(do(http.MethodGet, "/auth/whoami", "", sess), "whoami")
			wantDisabled(do(http.MethodPost, "/master-key/generate", "", sess), "POST to another route")
			wantDisabled(do(http.MethodGet, "/auth/logout", "", sess), "GET logout")
			wantStatus(do(http.MethodGet, "/test/caller", "", yamlSess), http.StatusOK, "session with no users row")
			wantStatus(do(http.MethodGet, "/test/caller", "test-admin-token", nil), http.StatusOK, "admin token")
			wantStatus(do(http.MethodGet, "/test/caller", "test-admin-token", sess), http.StatusOK, "admin token beside the disabled session")

			rows.setDisabled(t, cat)
			wantStatus(do(http.MethodGet, "/test/caller", "", sess), http.StatusOK, "re-enabled account")

			rows.setDisabled(t, cat, "u-1")
			if rec := do(http.MethodPost, "/auth/logout", "", sess); rec.Code >= 300 {
				t.Fatalf("logout of a disabled session = %d: %s", rec.Code, rec.Body)
			}
			rows.setDisabled(t, cat)
			wantStatus(do(http.MethodGet, "/test/caller", "", sess), http.StatusUnauthorized, "session after logout")
		})
	}
}
