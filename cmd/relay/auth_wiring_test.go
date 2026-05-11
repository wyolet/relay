package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/internal/auth"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/reqid"
)

// buildTestRouter mirrors the production mount order with the supplied key set.
// Only /healthz, /v1/chat/completions (stubbed) are registered — enough for the smoke test.
func buildTestRouter(keys [][]byte) http.Handler {
	authMW := auth.Middleware(keys, nil)

	r := chi.NewRouter()
	r.Use(reqid.Middleware(slog.Default()))
	r.Use(httpmw.LimitBody(httpmw.DefaultMaxRequestBytes))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	r.Group(func(r chi.Router) {
		r.Use(authMW)
		r.Post("/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	})

	return r
}

func TestAuthWiring_HealthzOpenWithoutBearer(t *testing.T) {
	h := buildTestRouter([][]byte{[]byte("secret")})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz without bearer: got %d want 200", rec.Code)
	}
}

func TestAuthWiring_ChatCompletions401WithoutBearer(t *testing.T) {
	h := buildTestRouter([][]byte{[]byte("secret")})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/v1/chat/completions without bearer: got %d want 401", rec.Code)
	}
}

func TestAuthWiring_ChatCompletions200WithBearer(t *testing.T) {
	h := buildTestRouter([][]byte{[]byte("secret")})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/chat/completions with valid bearer: got %d want 200", rec.Code)
	}
}
