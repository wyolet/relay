package control

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// Rotating the master key over HTTP is gone: data keys rotate in the database and the key-encryption key changes through the deployment env. Generating a candidate key stays.
func TestMasterKeyRotateIsGone(t *testing.T) {
	r := chi.NewRouter()
	Mount(r, mountDeps(t))
	for path, want := range map[string]int{
		"/master-key/rotate":   http.StatusNotFound,
		"/master-key/generate": http.StatusOK,
	} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Authorization", "Bearer test-admin-token")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("POST %s = %d, want %d: %s", path, rec.Code, want, rec.Body)
		}
	}
}
