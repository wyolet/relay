package session

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/pkg/kv"
)

// The actor's SessionID ends up in audit rows readable by lower-privileged
// principals, so it must identify the session without being its credential.
func TestActorSessionIDDoesNotAuthenticate(t *testing.T) {
	m := New(kv.NewMem(), false, "sess:")

	login := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := m.Login(r.Context(), "u-admin", "admin", "admin"); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	rec := httptest.NewRecorder()
	login.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/auth/login", nil))
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value == "" {
		t.Fatalf("no %s cookie set on login: %v", cookieName, rec.Result().Cookies())
	}

	stampedOn := func(c *http.Cookie) *actor.Actor {
		var got *actor.Actor
		h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = actor.From(r.Context())
			w.WriteHeader(http.StatusNoContent)
		}))
		req := httptest.NewRequest(http.MethodGet, "/api/auth/whoami", nil)
		req.AddCookie(c)
		h.ServeHTTP(httptest.NewRecorder(), req)
		return got
	}

	first := stampedOn(cookie)
	if first == nil || first.UserID != "u-admin" {
		t.Fatalf("session cookie did not authenticate: %+v", first)
	}
	if first.SessionID == "" {
		t.Fatal("actor.SessionID is empty, want a correlation id")
	}
	if first.SessionID == cookie.Value {
		t.Fatal("actor.SessionID is the session cookie token")
	}
	if again := stampedOn(cookie); again == nil || again.SessionID != first.SessionID {
		t.Fatalf("actor.SessionID not stable across requests: %q vs %+v", first.SessionID, again)
	}
	if replayed := stampedOn(&http.Cookie{Name: cookieName, Value: first.SessionID}); replayed != nil {
		t.Fatalf("replaying actor.SessionID as the cookie authenticated as %+v", replayed)
	}
}
