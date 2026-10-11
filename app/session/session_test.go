package session

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/auth/principal"
	"github.com/wyolet/relay/pkg/kv"
)

// failStore returns err from every kv operation — simulates a down/slow
// backend (e.g. context deadline exceeded against valkey).
type failStore struct {
	err error
}

func (f *failStore) Get(context.Context, string) ([]byte, error)              { return nil, f.err }
func (f *failStore) Set(context.Context, string, []byte, time.Duration) error { return f.err }
func (f *failStore) Del(context.Context, string) error                        { return f.err }
func (f *failStore) Incr(context.Context, string, int64) (int64, error)       { return 0, f.err }
func (f *failStore) Expire(context.Context, string, time.Duration) error      { return f.err }
func (f *failStore) Range(context.Context, string) ([]kv.Entry, error)        { return nil, f.err }
func (f *failStore) WithLock(ctx context.Context, _ []string, fn func(context.Context) error) error {
	return f.err
}
func (f *failStore) Close() error { return nil }

func TestMiddlewareStoreFailure(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	defer slog.SetDefault(prev)

	m := New(&failStore{err: context.DeadlineExceeded}, false, "sess:")
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/hosts", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "some-token"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("content-type = %q, want application/problem+json", ct)
	}
	if !strings.Contains(rec.Body.String(), `"status":500`) {
		t.Errorf("body = %q, want problem+json error", rec.Body.String())
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "session middleware failure") {
		t.Errorf("log = %q, want attributable message", logged)
	}
	if !strings.Contains(logged, "session find (kv get)") {
		t.Errorf("log = %q, want op-named wrapped error", logged)
	}
	if !strings.Contains(logged, "/api/hosts") {
		t.Errorf("log = %q, want request path attribute", logged)
	}
	if !strings.Contains(logged, `"level":"ERROR"`) {
		t.Errorf("log = %q, want ERROR level", logged)
	}
}

func TestMiddlewareHealthyStore(t *testing.T) {
	m := New(kv.NewMem(), false, "sess:")

	var loggedIn bool
	login := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := m.Login(r.Context(), "u-1", "admin"); err != nil {
			t.Fatalf("Login: %v", err)
		}
		loggedIn = true
	}))
	req := httptest.NewRequest("POST", "/auth/login", nil)
	rec := httptest.NewRecorder()
	login.ServeHTTP(rec, req)
	if !loggedIn || rec.Code != http.StatusOK {
		t.Fatalf("login: code=%d loggedIn=%v", rec.Code, loggedIn)
	}

	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no session cookie set on login")
	}

	var errFind error
	_, found, errFind := (&kvStore{kv: kv.NewMem(), prefix: "sess:"}).Find("missing")
	if errFind != nil || found {
		t.Fatalf("Find(missing) = found=%v err=%v, want miss with nil error", found, errFind)
	}
}

// LoginOIDC persists the IdP subject, sid and groups alongside the normal
// payload, and the session round-trips like a password login.
func TestLoginOIDC_StoresSubjectSidAndGroups(t *testing.T) {
	store := kv.NewMem()
	m := New(store, false, "sess:")

	login := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := m.LoginOIDC(r.Context(), "u-1", "alice", "https://idp|sub-1", "sid-42", []string{"platform-eng"}, "admin"); err != nil {
			t.Fatalf("LoginOIDC: %v", err)
		}
	}))
	rec := httptest.NewRecorder()
	login.ServeHTTP(rec, httptest.NewRequest("GET", "/auth/callback", nil))

	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no session cookie set")
	}

	// The committed scs payload (gob-encoded) must carry both keys and both
	// values — and the actor must round-trip on the next request.
	raw, found, err := (&kvStore{kv: store, prefix: "sess:"}).Find(cookies[0].Value)
	if err != nil || !found {
		t.Fatalf("committed session not found: %v", err)
	}
	for _, want := range []string{"oidc_subject", "oidc_sid", "idp_groups", "https://idp|sub-1", "sid-42", "platform-eng"} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("session payload missing %q", want)
		}
	}

	p := principalFor(t, m, cookies[0])
	if p == nil || p.ID != "u-1" || p.Username != "alice" || p.Kind != principal.KindUser || p.Credential.Kind != principal.CredentialSession {
		t.Errorf("principal did not round-trip: %+v", p)
	}
	if p == nil || len(p.Groups) != 1 || p.Groups[0] != "platform-eng" {
		t.Errorf("IdP groups did not round-trip: %+v", p)
	}
	if p == nil || p.Issuer != "https://idp" || p.Subject != "sub-1" {
		t.Errorf("IdP identity did not round-trip: %+v", p)
	}
	if p == nil || !slices.Equal(p.Subjects, []string{"user:u-1", "group:platform-eng", "group:system:authenticated"}) || !slices.Equal(p.Roles, []string{"admin"}) {
		t.Errorf("subjects and roles did not round-trip: %+v", p)
	}
}

func TestNewWithGroupsAddsLocalGroups(t *testing.T) {
	m := NewWithGroups(kv.NewMem(), false, "sess:", func(userID string) []string { return []string{"local-of-" + userID} })
	p := principalFor(t, m, loginCookie(t, m, "u-1"))
	if p == nil || !slices.Equal(p.Subjects, []string{"user:u-1", "group:local-of-u-1", "group:system:authenticated"}) {
		t.Errorf("principal = %+v", p)
	}
}

// A request with no session, or a session that names no user, is not the session's to claim.
func TestAuthenticateLeavesAnonymousRequests(t *testing.T) {
	m := New(kv.NewMem(), false, "sess:")
	if p := principalFor(t, m, nil); p != nil {
		t.Errorf("no cookie authenticated as %+v", p)
	}
	if p := principalFor(t, m, &http.Cookie{Name: cookieName, Value: "unknown"}); p != nil {
		t.Errorf("unknown token authenticated as %+v", p)
	}
	if p := principalFor(t, m, loginCookie(t, m, "")); p != nil {
		t.Errorf("a session with no user id authenticated as %+v", p)
	}
}

// commitFailStore reads fine but fails writes — exercises scs's mid-response
// commit-error path (ErrorFunc fires from inside WriteHeader).
type commitFailStore struct {
	kv.Store
	err error
}

// Set fails session commits only; Login's per-user index write goes through
// so the handler reaches the commit this test is about.
func (c *commitFailStore) Set(ctx context.Context, key string, v []byte, ttl time.Duration) error {
	if strings.HasPrefix(key, "sess:user:") {
		return c.Store.Set(ctx, key, v, ttl)
	}
	return c.err
}

func TestCommitFailure_CleanErrorResponse(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	defer slog.SetDefault(prev)

	m := New(&commitFailStore{Store: kv.NewMem(), err: context.DeadlineExceeded}, false, "sess:")
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := m.Login(r.Context(), "u-1", "admin"); err != nil {
			t.Fatalf("Login: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/auth/login", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (commit failed before headers flushed)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"status":500`) || strings.Contains(body, `"ok":true`) {
		t.Errorf("body = %q, want error payload with NO trailing handler body", body)
	}
	if !strings.Contains(logBuf.String(), "session commit (kv set)") {
		t.Errorf("log = %q, want op-named commit error", logBuf.String())
	}
}

func TestDuplicateWriteHeader_SuppressedAndAttributed(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	defer slog.SetDefault(prev)

	m := New(kv.NewMem(), false, "sess:")
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.WriteHeader(http.StatusTeapot) // buggy double write
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/buggy", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want first write (200) to win", rec.Code)
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "duplicate WriteHeader suppressed") || !strings.Contains(logged, "/api/buggy") {
		t.Errorf("log = %q, want suppression warn with offender path", logged)
	}
}
