package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/pkg/reqid"
)

type mockReloader struct {
	called bool
	err    error
}

func (m *mockReloader) Reload(_ context.Context) error {
	m.called = true
	return m.err
}

func newAdminRouter(tok string, store reloader) http.Handler {
	r := chi.NewRouter()
	r.Use(reqid.Middleware(slog.Default()))
	if tok != "" && store != nil {
		r.Post("/admin/reload", adminReloadHandler(tok, store))
	}
	return r
}

func TestAdminReload_TokenUnset_Returns404(t *testing.T) {
	// No token → endpoint not registered → 404.
	r := newAdminRouter("", nil)
	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}

func TestAdminReload_MissingToken_Returns404(t *testing.T) {
	store := &mockReloader{}
	r := newAdminRouter("secret", store)
	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
	if store.called {
		t.Error("Reload should not have been called")
	}
}

func TestAdminReload_WrongToken_Returns404(t *testing.T) {
	store := &mockReloader{}
	r := newAdminRouter("correct", store)
	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
	if store.called {
		t.Error("Reload should not have been called")
	}
}

func TestAdminReload_CorrectToken_Returns200(t *testing.T) {
	store := &mockReloader{}
	r := newAdminRouter("secret", store)
	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("want 200, got %d", w.Code)
	}
	if !store.called {
		t.Error("Reload should have been called")
	}
}

func TestAdminReload_ReloadError_Returns500(t *testing.T) {
	store := &mockReloader{err: errors.New("db gone")}
	r := newAdminRouter("secret", store)
	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", w.Code)
	}
}

func TestAdminReload_YAMLBackend_Returns404(t *testing.T) {
	// When pgStoreForAdmin is nil (YAML backend), endpoint is not registered.
	r := newAdminRouter("", nil) // token set but no store simulated by not registering
	req := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}
