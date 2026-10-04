package session

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wyolet/relay/app/actor"
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

func actorFor(m *Manager, c *http.Cookie) *actor.Actor {
	var got *actor.Actor
	h := m.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = actor.From(r.Context()) }))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(c)
	h.ServeHTTP(httptest.NewRecorder(), req)
	return got
}

// DestroyUser ends every session of one account and no one else's.
func TestDestroyUserEndsOnlyThatUsersSessions(t *testing.T) {
	m := New(kv.NewMem(), false, "sess:")
	a1, a2, b := loginCookie(t, m, "u-a"), loginCookie(t, m, "u-a"), loginCookie(t, m, "u-b")
	if err := m.DestroyUser(context.Background(), "u-a"); err != nil {
		t.Fatalf("DestroyUser: %v", err)
	}
	for i, c := range []*http.Cookie{a1, a2} {
		if a := actorFor(m, c); a != nil {
			t.Fatalf("session %d of u-a still authenticates as %+v", i, a)
		}
	}
	if a := actorFor(m, b); a == nil || a.UserID != "u-b" {
		t.Fatalf("u-b's session = %+v, want it intact", a)
	}
}
