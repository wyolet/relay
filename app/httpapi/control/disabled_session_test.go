package control

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/auth/authn"
	"github.com/wyolet/relay/auth/principal"
	"github.com/wyolet/relay/pkg/kv"
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

type failingSessionEnder struct{ destroyed, loggedOut int }

func (f *failingSessionEnder) DestroyUser(context.Context, string) error {
	f.destroyed++
	return errors.New("kv down")
}

func (f *failingSessionEnder) Logout(context.Context) error {
	f.loggedOut++
	return errors.New("kv down")
}

// Ending the sessions is best effort; the request is refused whether or not it succeeds, and an active or non-session principal ends nothing.
func TestAdmitOrEndSessionRefusesWhenTheEndFails(t *testing.T) {
	ender := &failingSessionEnder{}
	admit := admitOrEndSession(ender)
	disabled := &principal.Principal{ID: "u-1", Kind: principal.KindUser, Status: principal.StatusDisabled, Credential: principal.Credential{Kind: principal.CredentialSession}}
	if err := admit(t.Context(), disabled); !errors.Is(err, authn.ErrInactive) {
		t.Fatalf("admit = %v, want ErrInactive", err)
	}
	if ender.destroyed != 1 || ender.loggedOut != 1 {
		t.Fatalf("DestroyUser %d, Logout %d calls, want one each", ender.destroyed, ender.loggedOut)
	}
	active := &principal.Principal{ID: "u-2", Kind: principal.KindUser, Status: principal.StatusActive, Credential: principal.Credential{Kind: principal.CredentialSession}}
	if err := admit(t.Context(), active); err != nil {
		t.Fatalf("admit active = %v", err)
	}
	unresolved := &principal.Principal{Kind: principal.KindAdmin, Credential: principal.Credential{Kind: principal.CredentialService}}
	if err := admit(t.Context(), unresolved); !errors.Is(err, authn.ErrInactive) {
		t.Fatalf("admit unresolved = %v, want ErrInactive", err)
	}
	if ender.destroyed != 1 || ender.loggedOut != 1 {
		t.Fatalf("a non-refused or non-session principal ended sessions: DestroyUser %d, Logout %d", ender.destroyed, ender.loggedOut)
	}
}

const (
	accountDisabledBody = `{"error":{"type":"authentication_error","code":"account_disabled","message":"account is disabled; contact your administrator"}}`
	unauthenticatedBody = `{"error":{"type":"authentication_error","message":"unauthenticated"}}`
)

// A session can outlive its account's disable (the session purge failed, a login raced it, or the row was changed outside the stores). Its next control request, on any route, ends every session of that account, expires the cookie and answers account_disabled; after that the browser is simply signed out. All of it reads the snapshot alone.
func TestControlEndsDisabledAccountSession(t *testing.T) {
	for _, prefix := range []string{"", "/api"} {
		t.Run("mounted at "+prefix+"/", func(t *testing.T) {
			users := &disabledUserSource{}
			cat := catalogtest.Catalog{}.New()
			cat.UseTokenVersions(users)
			if err := cat.Reload(t.Context()); err != nil {
				t.Fatalf("reload: %v", err)
			}
			h, store := authHarnessAt(t, prefix, cat)
			sess := loginAtPrefix(t, h, prefix, `{"userId":"u-1"}`)
			other := loginAtPrefix(t, h, prefix, `{"userId":"u-1"}`)
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
			want := func(rec *httptest.ResponseRecorder, status int, body, what string) {
				t.Helper()
				if rec.Code != status {
					t.Fatalf("%s = %d, want %d: %s", what, rec.Code, status, rec.Body)
				}
				if body != "" && rec.Body.String() != body {
					t.Fatalf("%s body = %s, want %s", what, rec.Body, body)
				}
			}
			stored := func(key string) bool {
				t.Helper()
				_, err := store.Get(context.Background(), key)
				if err != nil && !errors.Is(err, kv.ErrNotFound) {
					t.Fatalf("kv get %s: %v", key, err)
				}
				return err == nil
			}

			users.setDisabled(t, cat, "u-1")
			// The admin token outranks the session, so the session is never evaluated and survives.
			want(do(http.MethodGet, "/test/caller", "test-admin-token", sess), http.StatusOK, "", "admin token beside the disabled session")
			if !stored(sessionKVKey(sess.Value)) {
				t.Fatal("the admin token's request ended the session it outranked")
			}
			want(do(http.MethodGet, "/test/caller", "", yamlSess), http.StatusOK, "", "session with no users row")

			rec := do(http.MethodGet, "/test/caller", "", sess)
			want(rec, http.StatusUnauthorized, accountDisabledBody, "disabled account's session")
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("content-type = %q, want application/json", ct)
			}
			expired := false
			for _, c := range rec.Result().Cookies() {
				expired = expired || (c.Name == "relay_session" && c.MaxAge < 0)
			}
			if !expired {
				t.Errorf("response cookies %v, want relay_session expired", rec.Result().Cookies())
			}
			for _, c := range []*http.Cookie{sess, other} {
				if stored(sessionKVKey(c.Value)) || stored(sessionIndexKVKey("u-1", c.Value)) {
					t.Fatalf("session %s or its index entry survived", c.Value)
				}
			}

			want(do(http.MethodGet, "/test/caller", "", sess), http.StatusUnauthorized, unauthenticatedBody, "the same cookie again")
			want(do(http.MethodGet, "/test/caller", "", other), http.StatusUnauthorized, unauthenticatedBody, "the account's other session")
			want(do(http.MethodGet, "/test/caller", "test-admin-token", nil), http.StatusOK, "", "admin token")

			// The browser that held the ended session can sign in as someone else.
			bob := loginAtPrefix(t, h, prefix, `{"userId":"u-2"}`)
			want(do(http.MethodGet, "/test/caller", "", bob), http.StatusOK, "", "another account's login")

			users.setDisabled(t, cat)
			fresh := loginAtPrefix(t, h, prefix, `{"userId":"u-1"}`)
			want(do(http.MethodGet, "/test/caller", "", fresh), http.StatusOK, "", "a fresh login after re-enable")
		})
	}
}
