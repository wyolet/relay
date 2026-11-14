package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/go-cmp/cmp"
)

type stubPinger struct{ err error }

func (p stubPinger) Ping(context.Context) error { return p.err }

// Readiness is the backends' health and nothing of why one failed: the endpoint is public, so the cause goes to the log. Liveness ignores the backends. Whether a master key is set is posture detail for a signed-in caller alone.
func TestHealth(t *testing.T) {
	down := errors.New("dial tcp 10.0.0.5:5432: refused")
	for _, tc := range []struct {
		name      string
		checks    map[string]Pinger
		path      string
		signedIn  bool
		masterKey bool
		status    int
		want      map[string]any
	}{
		{"every backend up", map[string]Pinger{"pg": stubPinger{}, "redis": stubPinger{}}, "/healthz", false, true, 200,
			map[string]any{"status": "ok", "backends": map[string]any{"pg": "ok", "redis": "ok"}}},
		{"one backend down", map[string]Pinger{"pg": stubPinger{err: down}, "redis": stubPinger{}}, "/healthz", false, true, 503,
			map[string]any{"status": "degraded", "backends": map[string]any{"pg": "error", "redis": "ok"}}},
		{"no backends to ask", nil, "/healthz", false, true, 200, map[string]any{"status": "ok", "backends": map[string]any{}}},
		{"signed in, master key set", nil, "/healthz", true, true, 200, map[string]any{"status": "ok", "backends": map[string]any{}, "master_key_configured": true}},
		{"signed in, no master key", nil, "/healthz", true, false, 200, map[string]any{"status": "ok", "backends": map[string]any{}, "master_key_configured": false}},
		{"signed in, a backend down", map[string]Pinger{"pg": stubPinger{err: down}}, "/healthz", true, true, 503,
			map[string]any{"status": "degraded", "backends": map[string]any{"pg": "error"}, "master_key_configured": true}},
		{"liveness with a backend down", map[string]Pinger{"pg": stubPinger{err: down}}, "/livez", false, true, 200, map[string]any{"status": "ok"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := mountDeps(t)
			d.HealthChecks, d.MasterKeyConfigured = tc.checks, tc.masterKey
			r := chi.NewRouter()
			Mount(r, d)
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.signedIn {
				req.Header.Set("Authorization", "Bearer "+d.AdminToken)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			var got map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != tc.status {
				t.Fatalf("status = %d (%v), want %d: %s", w.Code, err, tc.status, w.Body)
			}
			delete(got, "$schema")
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("body (-want +got):\n%s", diff)
			}
		})
	}
}

// The UI generates its client from the spec, so the 503 must carry the same body schema as the 200.
func TestHealthzSpecDeclaresBothBodies(t *testing.T) {
	doc := controlDoc(t)
	for _, code := range []string{"200", "503"} {
		if ref := responseRef(operation(t, doc, "/healthz", "get"), code); ref != "#/components/schemas/ControlHealth" {
			t.Errorf("%s schema = %q, want ControlHealth", code, ref)
		}
	}
}
