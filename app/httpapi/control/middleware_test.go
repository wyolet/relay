package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"github.com/google/go-cmp/cmp"

	"errors"
	"sync"

	"github.com/wyolet/relay/app/actor"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/session"
	"github.com/wyolet/relay/auth/authn"
	"github.com/wyolet/relay/auth/principal"
	"github.com/wyolet/relay/pkg/kv"
)

type callerOutput struct {
	Body struct {
		UserID     string   `json:"userId"`
		Username   string   `json:"username"`
		SessionID  string   `json:"sessionId"`
		AdminToken bool     `json:"adminToken"`
		Roles      []string `json:"roles"`
		Subjects   []string `json:"subjects"`
		IdPGroups  []string `json:"idpGroups"`
	}
}

type loginAsInput struct {
	Body struct {
		UserID string   `json:"userId"`
		Groups []string `json:"groups,omitempty"`
	}
}

// authHarness mounts the control API with its real session and admin-token wiring plus two test operations: one that logs a session in and one, protected, that echoes the caller every handler sees.
func authHarness(t *testing.T) (http.Handler, kv.Store) {
	t.Helper()
	return authHarnessAt(t, "", nil)
}

// authHarnessAt is authHarness with the API mounted under prefix, the way the composition root mounts it under /api, and with cat as the catalog when non-nil.
func authHarnessAt(t *testing.T, prefix string, cat *appcatalog.Catalog) (http.Handler, kv.Store) {
	t.Helper()
	store := kv.NewMem()
	t.Cleanup(func() { _ = store.Close() })
	d := mountDeps(t)
	d.Sessions = session.New(store, false)
	if cat != nil {
		// The test catalog carries its own users and no users table stands behind it, so the store has nothing to confirm.
		d.Catalog, d.Users = cat, nil
	}
	r := chi.NewRouter()
	var api huma.API
	if prefix == "" {
		api = Mount(r, d)
	} else {
		r.Route(prefix, func(sub chi.Router) { api = Mount(sub, d) })
	}
	huma.Register(api, huma.Operation{OperationID: "test_login_as", Method: http.MethodPost, Path: "/test/login-as"},
		func(ctx context.Context, in *loginAsInput) (*struct{}, error) {
			if len(in.Body.Groups) > 0 {
				return nil, d.Sessions.LoginOIDC(ctx, in.Body.UserID, "alice", "https://idp|sub-1", "sid-1", in.Body.Groups, "admin")
			}
			return nil, d.Sessions.Login(ctx, in.Body.UserID, "alice", "admin")
		})
	huma.Register(api, huma.Operation{OperationID: "test_caller", Method: http.MethodGet, Path: "/test/caller",
		Middlewares: huma.Middlewares{httpapi.HumaAuth(RequireActor)}},
		func(ctx context.Context, _ *struct{}) (*callerOutput, error) {
			a := actor.From(ctx)
			out := &callerOutput{}
			out.Body.UserID, out.Body.Username, out.Body.SessionID, out.Body.AdminToken = a.UserID, a.Username, a.SessionID, a.AdminToken
			out.Body.Roles, out.Body.Subjects, out.Body.IdPGroups = a.Roles, a.Subjects, a.IdPGroups
			return out, nil
		})
	return r, store
}

func loginAs(t *testing.T, h http.Handler, body string) *http.Cookie {
	t.Helper()
	return loginAtPrefix(t, h, "", body)
}

func loginAtPrefix(t *testing.T, h http.Handler, prefix, body string) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, prefix+"/test/login-as", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code >= 300 {
		t.Fatalf("login-as = %d: %s", rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "relay_session" {
			return c
		}
	}
	t.Fatal("login-as set no session cookie")
	return nil
}

func derivedSessionID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:16])
}

// sessionKVKey and sessionIndexKVKey spell out app/session's kv layout for tests that inspect the store directly.
func sessionKVKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "{sess:" + hex.EncodeToString(sum[:]) + "}:data"
}

func sessionIndexKVKey(userID, token string) string {
	sum := sha256.Sum256([]byte(token))
	return "{sessuser:" + userID + "}:" + hex.EncodeToString(sum[:])
}

// The control plane's authentication outcomes: which caller each credential yields, which requests are refused and how, and that the admin token outranks a session.
func TestControlAuthOutcomes(t *testing.T) {
	h, store := authHarness(t)
	sess := loginAs(t, h, `{"userId":"u-1"}`)
	oidc := loginAs(t, h, `{"userId":"u-2","groups":["platform-eng","system:admins"]}`)

	expired := &http.Cookie{Name: "relay_session", Value: "expired-token"}
	if err := store.Set(context.Background(), sessionKVKey("expired-token"), []byte(`{"d":"","e":"2000-01-01T00:00:00Z"}`), time.Hour); err != nil {
		t.Fatal(err)
	}

	sessionCaller := callerOutput{}
	sessionCaller.Body.UserID, sessionCaller.Body.Username, sessionCaller.Body.SessionID = "u-1", "alice", derivedSessionID(sess.Value)
	sessionCaller.Body.Roles, sessionCaller.Body.Subjects = []string{"admin"}, []string{"user:u-1", "group:system:authenticated"}

	oidcCaller := callerOutput{}
	oidcCaller.Body.UserID, oidcCaller.Body.Username, oidcCaller.Body.SessionID = "u-2", "alice", derivedSessionID(oidc.Value)
	oidcCaller.Body.Roles, oidcCaller.Body.IdPGroups = []string{"admin"}, []string{"platform-eng", "system:admins"}
	oidcCaller.Body.Subjects = []string{"user:u-2", "group:platform-eng", "group:system:authenticated"}

	adminCaller := callerOutput{}
	adminCaller.Body.Username, adminCaller.Body.AdminToken = "admin-token", true

	tests := []struct {
		name   string
		header string
		cookie *http.Cookie
		status int
		want   *callerOutput
	}{
		{"no credential", "", nil, http.StatusUnauthorized, nil},
		{"unknown session", "", &http.Cookie{Name: "relay_session", Value: "no-such-token"}, http.StatusUnauthorized, nil},
		{"expired session", "", expired, http.StatusUnauthorized, nil},
		{"valid session", "", sess, http.StatusOK, &sessionCaller},
		{"idp session", "", oidc, http.StatusOK, &oidcCaller},
		{"admin token", "Bearer test-admin-token", nil, http.StatusOK, &adminCaller},
		{"wrong admin token", "Bearer nope", nil, http.StatusUnauthorized, nil},
		{"admin token wrong scheme", "bearer test-admin-token", nil, http.StatusUnauthorized, nil},
		{"empty bearer", "Bearer ", nil, http.StatusUnauthorized, nil},
		{"wrong admin token falls to the session", "Bearer nope", sess, http.StatusOK, &sessionCaller},
		{"admin token outranks a session", "Bearer test-admin-token", sess, http.StatusOK, &adminCaller},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/test/caller", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			if tt.cookie != nil {
				req.AddCookie(tt.cookie)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.status, rec.Body)
			}
			if len(rec.Result().Cookies()) != 0 {
				t.Errorf("response set cookies %v, want none", rec.Result().Cookies())
			}
			if tt.want == nil {
				if got, ct := rec.Body.String(), rec.Header().Get("Content-Type"); got != unauthenticatedBody || ct != "application/json" {
					t.Errorf("answered %s as %q, want %s as application/json", got, ct, unauthenticatedBody)
				}
				return
			}
			var got callerOutput
			if err := json.Unmarshal(rec.Body.Bytes(), &got.Body); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(*tt.want, got); diff != "" {
				t.Errorf("caller (-want +got):\n%s", diff)
			}
		})
	}
}

// whoami answers for itself rather than through the protected-route guard, and so do the public routes: an unauthenticated caller reaches them.
func TestControlAuthPublicRoutes(t *testing.T) {
	h, _ := authHarness(t)
	sess := loginAs(t, h, `{"userId":"u-1"}`)
	tests := []struct {
		name, path, header string
		cookie             *http.Cookie
		status             int
		want               map[string]any
	}{
		{"whoami without a caller", "/auth/whoami", "", nil, http.StatusUnauthorized, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "not authenticated"}}},
		{"whoami with a session", "/auth/whoami", "", sess, http.StatusOK, map[string]any{"username": "alice", "user_id": "u-1", "roles": []any{"admin"}, "subjects": []any{"user:u-1", "group:system:authenticated"}}},
		{"whoami with the admin token", "/auth/whoami", "Bearer test-admin-token", nil, http.StatusOK, map[string]any{"username": "admin-token", "user_id": ""}},
		{"version without a caller", "/version", "", nil, http.StatusOK, map[string]any{"version": httpapi.Version}},
		{"version with a wrong admin token", "/version", "Bearer nope", nil, http.StatusOK, map[string]any{"version": httpapi.Version}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			if tt.cookie != nil {
				req.AddCookie(tt.cookie)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != tt.status {
				t.Fatalf("status = %d (%v), want %d: %s", rec.Code, err, tt.status, rec.Body)
			}
			delete(got, "$schema")
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("body (-want +got):\n%s", diff)
			}
		})
	}
}

// A session's account status is read from the snapshot, never from Postgres: mountDeps' user store has no pool, so a per-request lookup would fail this test.
func TestControlAuthSessionStatusReadsNoPostgres(t *testing.T) {
	h, _ := authHarness(t)
	sess := loginAs(t, h, `{"userId":"u-disabled"}`)
	req := httptest.NewRequest(http.MethodGet, "/test/caller", nil)
	req.AddCookie(sess)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
}

// A session store that cannot answer fails the request before any handler runs.
func TestControlAuthSessionStoreDown(t *testing.T) {
	d := mountDeps(t)
	d.Sessions = session.New(downStore{kv.NewMem()}, false)
	r := chi.NewRouter()
	Mount(r, d)
	req := httptest.NewRequest(http.MethodPost, "/master-key/generate", nil)
	req.AddCookie(&http.Cookie{Name: "relay_session", Value: "some-token"})
	req.Header.Set("Authorization", "Bearer test-admin-token")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("content-type = %q, want application/problem+json", ct)
	}
}

type downStore struct{ kv.Store }

// Every refusal answers in the control plane's JSON error shape; a disabled account gets the account_disabled answer login and token mint give it.
func TestRejectAuthentication(t *testing.T) {
	tests := []struct {
		err    error
		status int
		body   string
	}{
		{authn.ErrInvalidCredential, http.StatusUnauthorized, `{"error":{"type":"authentication_error","message":"Unauthorized"}}`},
		{authn.ErrUnavailable, http.StatusServiceUnavailable, `{"error":{"type":"authentication_error","message":"Service Unavailable"}}`},
		{authn.ErrInactive, http.StatusUnauthorized, accountDisabledBody},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		rejectAuthentication(rec, httptest.NewRequest(http.MethodGet, "/", nil), tt.err)
		if rec.Code != tt.status || rec.Body.String() != tt.body || rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%v: %d %s %q, want %d %s", tt.err, rec.Code, rec.Body, rec.Header().Get("Content-Type"), tt.status, tt.body)
		}
	}
}

func (downStore) Get(context.Context, string) ([]byte, error) { return nil, context.DeadlineExceeded }

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
	admit := admitOrEndSession(ender, func(context.Context, string) (bool, error) { return true, nil })
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

// The snapshot trails a re-enable by up to one NOTIFY while login reads the store, so a disabled verdict the store contradicts admits and ends nothing, and one the store cannot confirm refuses without ending anything.
func TestAdmitOrEndSessionConfirmsWithTheStore(t *testing.T) {
	ender := &failingSessionEnder{}
	disabled := func() *principal.Principal {
		return &principal.Principal{ID: "u-1", Kind: principal.KindUser, Status: principal.StatusDisabled, Credential: principal.Credential{Kind: principal.CredentialSession}}
	}

	stale := admitOrEndSession(ender, func(context.Context, string) (bool, error) { return false, nil })
	p := disabled()
	if err := stale(t.Context(), p); err != nil {
		t.Fatalf("admit with a stale snapshot = %v, want admitted", err)
	}
	if p.Status != principal.StatusActive {
		t.Errorf("status = %q, want active", p.Status)
	}

	unknown := admitOrEndSession(ender, func(context.Context, string) (bool, error) { return false, errors.New("pg down") })
	if err := unknown(t.Context(), disabled()); !errors.Is(err, authn.ErrInactive) {
		t.Fatalf("admit when the store is unreachable = %v, want ErrInactive", err)
	}
	if ender.destroyed != 0 || ender.loggedOut != 0 {
		t.Fatalf("sessions ended without a confirmed disable: DestroyUser %d, Logout %d", ender.destroyed, ender.loggedOut)
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
