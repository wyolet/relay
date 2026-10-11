package session

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/auth/authn"
	"github.com/wyolet/relay/auth/principal"
)

var _ authn.Authenticator = (*Manager)(nil)

// memStore is a Store in a map; set errFind or errCommit to fail those calls.
type memStore struct {
	mu        sync.Mutex
	data      map[string][]byte
	index     map[string]map[string]time.Duration
	errFind   error
	errCommit error
	errIndex  error
}

func newMemStore() *memStore {
	return &memStore{data: map[string][]byte{}, index: map[string]map[string]time.Duration{}}
}

func (s *memStore) Find(token string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.errFind != nil {
		return nil, false, s.errFind
	}
	b, ok := s.data[token]
	return b, ok, nil
}

func (s *memStore) Commit(token string, b []byte, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.errCommit != nil {
		return s.errCommit
	}
	s.data[token] = b
	return nil
}

func (s *memStore) Delete(token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, token)
	return nil
}

func (s *memStore) IndexUserToken(_ context.Context, userID, token string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.errIndex != nil {
		return s.errIndex
	}
	if s.index[userID] == nil {
		s.index[userID] = map[string]time.Duration{}
	}
	s.index[userID][token] = ttl
	return nil
}

func (s *memStore) UserTokens(_ context.Context, userID string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.errIndex != nil {
		return nil, s.errIndex
	}
	var tokens []string
	for t := range s.index[userID] {
		tokens = append(tokens, t)
	}
	return tokens, nil
}

func (s *memStore) DeleteUserToken(_ context.Context, userID, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.errIndex != nil {
		return s.errIndex
	}
	delete(s.data, token)
	delete(s.index[userID], token)
	return nil
}

func serve(m *Manager, c *http.Cookie, h http.HandlerFunc) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	m.Middleware(h).ServeHTTP(rec, req)
	return rec
}

func login(t *testing.T, m *Manager, fn func(context.Context) error) *http.Cookie {
	t.Helper()
	rec := serve(m, nil, func(_ http.ResponseWriter, r *http.Request) {
		if err := fn(r.Context()); err != nil {
			t.Fatalf("login: %v", err)
		}
	})
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login set cookies %v, want one", cookies)
	}
	return cookies[0]
}

func whoami(t *testing.T, m *Manager, c *http.Cookie) *principal.Principal {
	t.Helper()
	var got *principal.Principal
	serve(m, c, func(_ http.ResponseWriter, r *http.Request) {
		p, claimed, err := m.Authenticate(r)
		if err != nil || claimed != (p != nil) {
			t.Errorf("Authenticate = %+v, %v, %v", p, claimed, err)
		}
		got = p
	})
	return got
}

func TestOptionsDefaultsAndCookie(t *testing.T) {
	m := New(newMemStore(), Options{})
	c := login(t, m, func(ctx context.Context) error { return m.Login(ctx, "u", "u") })
	if c.Name != "session" || c.Path != "/" || !c.HttpOnly || c.Secure || c.SameSite != http.SameSiteStrictMode || c.MaxAge < 86400 || c.MaxAge > 86401 {
		t.Errorf("default cookie = %+v", c)
	}

	m = New(newMemStore(), Options{CookieName: "app", Secure: true, Lifetime: time.Hour})
	c = login(t, m, func(ctx context.Context) error { return m.Login(ctx, "u", "u") })
	if c.Name != "app" || !c.Secure || c.MaxAge < 3600 || c.MaxAge > 3601 {
		t.Errorf("configured cookie = %+v", c)
	}
}

func TestLoginRoundTripsAndRotatesToken(t *testing.T) {
	store := newMemStore()
	m := New(store, Options{Lifetime: time.Hour})
	first := login(t, m, func(ctx context.Context) error { return m.Login(ctx, "u-1", "alice") })
	second := login(t, m, func(ctx context.Context) error { return m.Login(ctx, "u-1", "alice", "admin") })
	if first.Value == second.Value {
		t.Fatal("login did not issue a fresh token")
	}
	if ttl := store.index["u-1"][second.Value]; ttl != time.Hour {
		t.Errorf("index ttl = %s, want the lifetime", ttl)
	}

	p := whoami(t, m, second)
	want := principal.Principal{ID: "u-1", Kind: principal.KindUser, Username: "alice", Roles: []string{"admin"}, SessionID: sessionID(second.Value), Credential: principal.Credential{Kind: principal.CredentialSession}}
	if p == nil || p.ID != want.ID || p.Kind != want.Kind || p.Username != want.Username || !slices.Equal(p.Roles, want.Roles) || p.SessionID != want.SessionID || p.Credential != want.Credential || p.Subjects != nil {
		t.Errorf("principal = %+v, want %+v", p, want)
	}
}

func TestLoginOIDCAndSubjects(t *testing.T) {
	m := New(newMemStore(), Options{Subjects: func(uid string, local, idp []string) []string {
		return append(append([]string{"user:" + uid}, local...), idp...)
	}})
	m.UseGroups(func(uid string) []string { return []string{"local-of-" + uid} })
	c := login(t, m, func(ctx context.Context) error {
		return m.LoginOIDC(ctx, "u-1", "alice", "https://idp|sub-1", "sid-1", []string{"eng"}, "viewer")
	})
	p := whoami(t, m, c)
	if p == nil || p.Issuer != "https://idp" || p.Subject != "sub-1" || !slices.Equal(p.Groups, []string{"eng"}) || !slices.Equal(p.Roles, []string{"viewer"}) {
		t.Fatalf("principal = %+v", p)
	}
	if !slices.Equal(p.Subjects, []string{"user:u-1", "local-of-u-1", "eng"}) {
		t.Errorf("subjects = %v", p.Subjects)
	}

	// Without UseGroups the hook still runs, with no local groups.
	m = New(newMemStore(), Options{Subjects: func(uid string, local, _ []string) []string { return append([]string{uid}, local...) }})
	c = login(t, m, func(ctx context.Context) error { return m.LoginOIDC(ctx, "u-2", "bob", "", "", nil) })
	if p := whoami(t, m, c); p == nil || !slices.Equal(p.Subjects, []string{"u-2"}) || p.Issuer != "" || p.Groups != nil {
		t.Errorf("principal = %+v", p)
	}
}

func TestAuthenticateLeavesAnonymous(t *testing.T) {
	m := New(newMemStore(), Options{})
	if p := whoami(t, m, nil); p != nil {
		t.Errorf("no cookie = %+v", p)
	}
	if p := whoami(t, m, &http.Cookie{Name: "session", Value: "unknown"}); p != nil {
		t.Errorf("unknown token = %+v", p)
	}
	c := login(t, m, func(ctx context.Context) error { return m.Login(ctx, "", "") })
	if p := whoami(t, m, c); p != nil {
		t.Errorf("session without user = %+v", p)
	}
}

// Stored audit rows carry this id and SQL recomputes it from stored tokens, so the derivation is fixed.
func TestSessionIDIsTruncatedSHA256(t *testing.T) {
	if got := sessionID("abc"); got != "ba7816bf8f01cfea414140de5dae2223" {
		t.Errorf("sessionID(abc) = %s", got)
	}
}

func TestLogoutAndDestroyUser(t *testing.T) {
	store := newMemStore()
	m := New(store, Options{})
	a1 := login(t, m, func(ctx context.Context) error { return m.Login(ctx, "a", "a") })
	a2 := login(t, m, func(ctx context.Context) error { return m.Login(ctx, "a", "a") })
	b := login(t, m, func(ctx context.Context) error { return m.Login(ctx, "b", "b") })

	serve(m, a1, func(_ http.ResponseWriter, r *http.Request) {
		if err := m.Logout(r.Context()); err != nil {
			t.Fatalf("Logout: %v", err)
		}
	})
	if p := whoami(t, m, a1); p != nil {
		t.Errorf("logged-out session = %+v", p)
	}

	if err := m.DestroyUser(context.Background(), "a"); err != nil {
		t.Fatalf("DestroyUser: %v", err)
	}
	if p := whoami(t, m, a2); p != nil {
		t.Errorf("destroyed session = %+v", p)
	}
	if len(store.index["a"]) != 0 {
		t.Errorf("index of a = %v, want empty", store.index["a"])
	}
	if p := whoami(t, m, b); p == nil || p.ID != "b" {
		t.Errorf("b's session = %+v, want intact", p)
	}
}

func TestStoreErrorsPropagate(t *testing.T) {
	errDown := errors.New("down")
	store := newMemStore()
	m := New(store, Options{})
	store.errIndex = errDown
	serve(m, nil, func(_ http.ResponseWriter, r *http.Request) {
		if err := m.Login(r.Context(), "u", "u"); !errors.Is(err, errDown) {
			t.Errorf("Login = %v, want the index error", err)
		}
		if err := m.LoginOIDC(r.Context(), "u", "u", "", "", nil); !errors.Is(err, errDown) {
			t.Errorf("LoginOIDC = %v, want the index error", err)
		}
	})
	if err := m.DestroyUser(context.Background(), "u"); !errors.Is(err, errDown) {
		t.Errorf("DestroyUser = %v, want the index error", err)
	}

	// A failing delete stops the walk.
	store = newMemStore()
	m = New(store, Options{})
	login(t, m, func(ctx context.Context) error { return m.Login(ctx, "u", "u") })
	failing := &deleteFailStore{memStore: store, err: errDown}
	m = New(failing, Options{})
	if err := m.DestroyUser(context.Background(), "u"); !errors.Is(err, errDown) {
		t.Errorf("DestroyUser = %v, want the delete error", err)
	}
}

type deleteFailStore struct {
	*memStore
	err error
}

func (s *deleteFailStore) DeleteUserToken(context.Context, string, string) error { return s.err }

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestFindFailureIsAttributed500(t *testing.T) {
	logged := captureLog(t)
	store := newMemStore()
	store.errFind = errors.New("find down")
	m := New(store, Options{})
	rec := serve(m, &http.Cookie{Name: "session", Value: "t"}, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("Content-Type") != "application/problem+json" || !strings.Contains(rec.Body.String(), `"status":500`) {
		t.Fatalf("response = %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if s := logged.String(); !strings.Contains(s, "session middleware failure") || !strings.Contains(s, "find down") || !strings.Contains(s, "/auth/login") {
		t.Errorf("log = %q", s)
	}
}

// On a commit failure scs calls ErrorFunc mid-response and then writes the handler's status; only the 500 may reach the client.
func TestCommitFailureDropsHandlerResponse(t *testing.T) {
	logged := captureLog(t)
	store := newMemStore()
	store.errCommit = errors.New("commit down")
	m := New(store, Options{})
	rec := serve(m, nil, func(w http.ResponseWriter, r *http.Request) {
		if err := m.Login(r.Context(), "u", "u"); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), `"ok":true`) || !strings.Contains(rec.Body.String(), `"status":500`) {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logged.String(), "commit down") {
		t.Errorf("log = %q", logged.String())
	}
}

func TestDuplicateWriteHeaderSuppressed(t *testing.T) {
	logged := captureLog(t)
	m := New(newMemStore(), Options{})
	rec := serve(m, nil, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		w.WriteHeader(http.StatusTeapot)
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want the first write", rec.Code)
	}
	if s := logged.String(); !strings.Contains(s, "duplicate WriteHeader suppressed") || !strings.Contains(s, "/auth/login") {
		t.Errorf("log = %q", s)
	}
}

func TestOnceWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	o := &onceWriter{ResponseWriter: rec, req: httptest.NewRequest(http.MethodGet, "/", nil)}
	if o.Unwrap() != rec {
		t.Error("Unwrap does not return the wrapped writer")
	}
	_, _ = o.Write([]byte("body"))
	o.fail(http.StatusInternalServerError, "text/plain", []byte("err"))
	if n, err := o.Write([]byte("more")); n != 4 || err != nil {
		t.Errorf("Write after fail = %d, %v; want swallowed", n, err)
	}
	if rec.Code != http.StatusOK || rec.Body.String() != "body" {
		t.Errorf("response = %d %q; a failure after the header went out writes nothing", rec.Code, rec.Body.String())
	}
}

// ErrorFunc on a writer the middleware did not wrap still answers with the problem body.
func TestErrorFuncOnPlainWriter(t *testing.T) {
	captureLog(t)
	m := New(newMemStore(), Options{})
	rec := httptest.NewRecorder()
	m.sm.ErrorFunc(rec, httptest.NewRequest(http.MethodGet, "/", nil), errors.New("x"))
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("Content-Type") != "application/problem+json" {
		t.Errorf("response = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}
