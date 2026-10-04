package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// mintServer stands in for the control listener over plain http: login sets
// a Secure session cookie, and mint answers only when that cookie comes
// back.
type mintServer struct {
	mu         sync.Mutex
	mintCookie string
	sessionEnd bool
}

func (m *mintServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "relay_session", Value: "s-1", Path: "/", Secure: true, HttpOnly: true})
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/api/auth/token", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.mintCookie = r.Header.Get("Cookie")
		m.mu.Unlock()
		if _, err := r.Cookie("relay_session"); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"no session"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"token":"ey.jwt","expiresAt":"2030-01-01T00:00:00Z"}`))
	})
	mux.HandleFunc("/api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.sessionEnd = true
		m.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func runMint(t *testing.T) *mintServer {
	t.Helper()
	m := &mintServer{}
	srv := httptest.NewServer(m.handler())
	t.Cleanup(srv.Close)
	t.Setenv("RELAY_PASSWORD", "pw")
	if err := runToken([]string{"mint", "--project", "p1", "--user", "u1", "--url", srv.URL}); err != nil {
		t.Fatalf("token mint: %v", err)
	}
	return m
}

func TestTokenMintSendsTheSessionCookieOverPlainHTTP(t *testing.T) {
	m := runMint(t)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mintCookie == "" {
		t.Fatal("mint carried no Cookie header")
	}
}

func TestTokenMintEndsTheSession(t *testing.T) {
	m := runMint(t)
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.sessionEnd {
		t.Fatal("the CLI left the session open")
	}
}
