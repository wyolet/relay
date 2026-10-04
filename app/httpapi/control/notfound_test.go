package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// The control API is mounted in front of the SPA fallback, which serves
// HTML for anything it does not recognise. TestUnknownControlRouteAnswersJSON
// pins that an unknown path under the API answers a parseable 404 — an older
// UI calling a renamed endpoint must see an error, not an index page.
func TestUnknownControlRouteAnswersJSON(t *testing.T) {
	deps := mountDeps(t)
	// The composition root's wiring: the API under /api, the SPA as the
	// listener-wide fallback behind it.
	root := chi.NewRouter()
	root.Route("/api", func(r chi.Router) { Mount(r, deps) })
	root.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>spa</html>"))
	})

	req := httptest.NewRequest(http.MethodGet, "/api/relay-keys", nil)
	req.Header.Set("Authorization", "Bearer "+deps.AdminToken)
	w := httptest.NewRecorder()
	root.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	var body struct {
		Error struct {
			Type, Code, Message string
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON (%v): %s", err, w.Body.String())
	}
	if body.Error.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", body.Error.Code)
	}
}
