package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

type stubPinger struct{ err error }

func (p stubPinger) Ping(context.Context) error { return p.err }

type healthBody struct {
	Status   string            `json:"status"`
	Backends map[string]string `json:"backends"`
}

// getUnauthenticated serves path through the full Mount chain (session, authenticate, audit) with no credential.
func getUnauthenticated(t *testing.T, p Pinger, path string) *httptest.ResponseRecorder {
	t.Helper()
	d := mountDeps(t)
	d.Pinger = p
	r := chi.NewRouter()
	Mount(r, d)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func decodeHealth(t *testing.T, w *httptest.ResponseRecorder) healthBody {
	t.Helper()
	var b healthBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body)
	}
	return b
}

func TestHealthzBackendOK(t *testing.T) {
	w := getUnauthenticated(t, stubPinger{}, "/healthz")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	b := decodeHealth(t, w)
	if b.Status != "ok" || b.Backends["pg"] != "ok" {
		t.Fatalf("body = %+v, want ok with pg ok", b)
	}
}

// A failing backend degrades readiness to 503 without leaking the cause to an unauthenticated caller.
func TestHealthzBackendFailureIs503(t *testing.T) {
	w := getUnauthenticated(t, stubPinger{err: errors.New("dial tcp 10.0.0.5:5432: refused")}, "/healthz")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", w.Code, w.Body)
	}
	b := decodeHealth(t, w)
	if b.Status != "degraded" || b.Backends["pg"] != "error" {
		t.Fatalf("body = %+v, want degraded with pg error", b)
	}
	if strings.Contains(w.Body.String(), "10.0.0.5") {
		t.Fatalf("body leaks the backend error: %s", w.Body)
	}
}

func TestHealthzNilPinger(t *testing.T) {
	w := getUnauthenticated(t, nil, "/healthz")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	b := decodeHealth(t, w)
	if b.Status != "ok" || len(b.Backends) != 0 {
		t.Fatalf("body = %+v, want ok with no backends", b)
	}
}

func TestLivezIgnoresBackend(t *testing.T) {
	w := getUnauthenticated(t, stubPinger{err: errors.New("pg down")}, "/livez")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
}
