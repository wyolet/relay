package session

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wyolet/relay/auth/principal"
	"github.com/wyolet/relay/pkg/kv"
)

func loginCookie(t *testing.T, m *Manager, userID string) *http.Cookie {
	t.Helper()
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := m.Login(r.Context(), userID, userID); err != nil {
			t.Fatalf("Login: %v", err)
		}
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/auth/login", nil))
	return rec.Result().Cookies()[0]
}

// principalFor is who a request carrying c authenticates as; nil when the session does not claim it.
func principalFor(t *testing.T, m *Manager, c *http.Cookie) *principal.Principal {
	t.Helper()
	var got *principal.Principal
	h := m.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		p, claimed, err := m.Authenticate(r)
		if err != nil || claimed != (p != nil) {
			t.Errorf("Authenticate = %+v, %v, %v", p, claimed, err)
		}
		got = p
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if c != nil {
		req.AddCookie(c)
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
	return got
}

// DestroyUser ends every session of one account and no one else's.
func TestDestroyUserEndsOnlyThatUsersSessions(t *testing.T) {
	m := New(kv.NewMem(), false)
	a1, a2, b := loginCookie(t, m, "u-a"), loginCookie(t, m, "u-a"), loginCookie(t, m, "u-b")
	if err := m.DestroyUser(context.Background(), "u-a"); err != nil {
		t.Fatalf("DestroyUser: %v", err)
	}
	for i, c := range []*http.Cookie{a1, a2} {
		if a := principalFor(t, m, c); a != nil {
			t.Fatalf("session %d of u-a still authenticates as %+v", i, a)
		}
	}
	if a := principalFor(t, m, b); a == nil || a.ID != "u-b" {
		t.Fatalf("u-b's session = %+v, want it intact", a)
	}
}
