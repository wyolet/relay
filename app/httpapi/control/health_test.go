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

// Whether a master key is set is posture detail: a signed-in caller sees it, an anonymous one doesn't.
func TestHealthzMasterKeyOnlyForAuthenticatedCallers(t *testing.T) {
	d := mountDeps(t)
	d.MasterKeyConfigured = true
	r := chi.NewRouter()
	Mount(r, d)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if strings.Contains(w.Body.String(), "master_key_configured") {
		t.Fatalf("anonymous body carries master_key_configured: %s", w.Body)
	}

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Authorization", "Bearer "+d.AdminToken)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var b struct {
		MasterKeyConfigured *bool `json:"master_key_configured"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil || b.MasterKeyConfigured == nil || !*b.MasterKeyConfigured {
		t.Fatalf("authenticated body = %s, want master_key_configured true", w.Body)
	}
}

// getUnauthenticated serves path through the full Mount chain (session, authenticate, audit) with no credential.
func getUnauthenticated(t *testing.T, checks map[string]Pinger, path string) *httptest.ResponseRecorder {
	t.Helper()
	d := mountDeps(t)
	d.HealthChecks = checks
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
	w := getUnauthenticated(t, map[string]Pinger{"pg": stubPinger{}}, "/healthz")
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
	w := getUnauthenticated(t, map[string]Pinger{"pg": stubPinger{err: errors.New("dial tcp 10.0.0.5:5432: refused")}, "redis": stubPinger{}}, "/healthz")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", w.Code, w.Body)
	}
	b := decodeHealth(t, w)
	if b.Status != "degraded" || b.Backends["pg"] != "error" || b.Backends["redis"] != "ok" {
		t.Fatalf("body = %+v, want degraded with pg error and redis ok", b)
	}
	if strings.Contains(w.Body.String(), "10.0.0.5") {
		t.Fatalf("body leaks the backend error: %s", w.Body)
	}
}

func TestHealthzNoChecks(t *testing.T) {
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
	w := getUnauthenticated(t, map[string]Pinger{"pg": stubPinger{err: errors.New("pg down")}}, "/livez")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
}

// The UI generates its client from the spec, so the 503 must carry the same body schema as the 200.
func TestHealthzSpecDeclaresBothBodies(t *testing.T) {
	w := getUnauthenticated(t, nil, "/openapi.json")
	var spec struct {
		Paths map[string]map[string]struct {
			Responses map[string]struct {
				Content map[string]struct {
					Schema struct {
						Ref string `json:"$ref"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &spec); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	responses := spec.Paths["/healthz"]["get"].Responses
	for _, code := range []string{"200", "503"} {
		if ref := responses[code].Content["application/json"].Schema.Ref; ref != "#/components/schemas/ControlHealth" {
			t.Errorf("%s schema = %q, want ControlHealth", code, ref)
		}
	}
}
