package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/session"
	"github.com/wyolet/relay/auth/authn"
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
	store := kv.NewMem()
	t.Cleanup(func() { _ = store.Close() })
	d := mountDeps(t)
	d.Sessions = session.New(store, false)
	r := chi.NewRouter()
	api := Mount(r, d)
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
	req := httptest.NewRequest(http.MethodPost, "/test/login-as", strings.NewReader(body))
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

// The control plane's authentication outcomes: which caller each credential yields, which requests are refused and how, and that the admin token outranks a session.
func TestControlAuthOutcomes(t *testing.T) {
	h, store := authHarness(t)
	sess := loginAs(t, h, `{"userId":"u-1"}`)
	oidc := loginAs(t, h, `{"userId":"u-2","groups":["platform-eng","system:admins"]}`)

	expired := &http.Cookie{Name: "relay_session", Value: "expired-token"}
	expiredSum := sha256.Sum256([]byte("expired-token"))
	if err := store.Set(context.Background(), "{sess:"+hex.EncodeToString(expiredSum[:])+"}:data", []byte(`{"d":"","e":"2000-01-01T00:00:00Z"}`), time.Hour); err != nil {
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
				if got, want := rec.Body.String(), `{"error":{"type":"authentication_error","message":"unauthenticated"}}`; got != want {
					t.Errorf("body = %s, want %s", got, want)
				}
				return
			}
			var got callerOutput
			if err := json.Unmarshal(rec.Body.Bytes(), &got.Body); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, *tt.want) {
				t.Errorf("caller = %+v, want %+v", got.Body, tt.want.Body)
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
	}{
		{"whoami without a caller", "/auth/whoami", "", nil, http.StatusUnauthorized},
		{"whoami with a session", "/auth/whoami", "", sess, http.StatusOK},
		{"whoami with the admin token", "/auth/whoami", "Bearer test-admin-token", nil, http.StatusOK},
		{"version without a caller", "/version", "", nil, http.StatusOK},
		{"version with a wrong admin token", "/version", "Bearer nope", nil, http.StatusOK},
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
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.status, rec.Body)
			}
		})
	}
}

// A session is honoured without consulting the account it names: relay ends a disabled user's sessions when the account is disabled, not on each request. mountDeps' user store has no pool, so a per-request lookup would fail this test.
func TestControlAuthSessionDoesNotReadAccountStatus(t *testing.T) {
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

// No control authenticator rejects today; a refusal still answers in the control plane's JSON error shape.
func TestRejectAuthentication(t *testing.T) {
	tests := []struct {
		err    error
		status int
		body   string
	}{
		{authn.ErrInvalidCredential, http.StatusUnauthorized, `{"error":{"type":"authentication_error","message":"Unauthorized"}}`},
		{authn.ErrUnavailable, http.StatusServiceUnavailable, `{"error":{"type":"authentication_error","message":"Service Unavailable"}}`},
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
