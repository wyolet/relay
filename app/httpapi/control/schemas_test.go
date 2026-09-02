package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/manifest"
)

func schemaServer(t *testing.T) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) { registerSchemas(r) })
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// Every kind cmd/catalog-schemas emits must be served, unauthenticated.
func TestSchemasEndpointServesEveryKind(t *testing.T) {
	srv := schemaServer(t)
	base := srv.URL + "/api/schemas/" + manifest.SchemaVersion

	resp, err := http.Get(base)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("index status %d", resp.StatusCode)
	}
	var index struct {
		Version string   `json:"version"`
		Kinds   []string `json:"kinds"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&index); err != nil {
		t.Fatalf("decode index: %v", err)
	}
	if index.Version != manifest.SchemaVersion || len(index.Kinds) == 0 {
		t.Fatalf("index = %+v", index)
	}
	// The kinds the loader can parse and the kinds we publish are the same
	// set; a new DTO without a regenerated schema fails here.
	want := []string{
		"Group", "Host", "HostKey", "Key", "Model", "Overlay", "Policy",
		"PolicyBinding", "Pricing", "Project", "Provider", "RateLimit",
		"Role", "RoleBinding", "ServiceAccount", "Team",
	}
	if !reflect.DeepEqual(index.Kinds, want) {
		t.Fatalf("kinds = %v, want %v", index.Kinds, want)
	}

	for _, kind := range index.Kinds {
		for _, suffix := range []string{"/" + kind, "/" + kind + ".schema.json"} {
			r, err := http.Get(base + suffix)
			if err != nil {
				t.Fatalf("get %s: %v", suffix, err)
			}
			body := map[string]any{}
			err = json.NewDecoder(r.Body).Decode(&body)
			r.Body.Close()
			if r.StatusCode != http.StatusOK {
				t.Fatalf("get %s: status %d", suffix, r.StatusCode)
			}
			if ct := r.Header.Get("Content-Type"); ct != "application/schema+json" {
				t.Fatalf("get %s: content-type %q", suffix, ct)
			}
			if err != nil {
				t.Fatalf("get %s: decode: %v", suffix, err)
			}
			if body["title"] != kind {
				t.Fatalf("get %s: title = %v", suffix, body["title"])
			}
		}
	}
}

func TestSchemasEndpointRejectsUnknown(t *testing.T) {
	srv := schemaServer(t)
	for _, path := range []string{
		"/api/schemas/" + manifest.SchemaVersion + "/NotAKind",
		"/api/schemas/v0alpha9/Team",
		"/api/schemas/v0alpha9",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("get %s: status %d, want 404", path, resp.StatusCode)
		}
	}
}
