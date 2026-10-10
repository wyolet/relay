package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/schemas"
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
		"Group", "Host", "HostBinding", "HostKey", "Key", "Model", "Overlay", "Policy",
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

// A closed set in the code is only a closed set to an editor if the schema
// says so; without the enum a typo in a role rule is caught at apply time
// instead of while writing the YAML.
func TestGeneratedSchemasCarryTheClosedSets(t *testing.T) {
	raw, err := schemas.FS.ReadFile("v1alpha2/Role.schema.json")
	if err != nil {
		t.Fatalf("read Role schema: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}

	rule := ruleSchema(t, doc)
	for field, want := range map[string][]string{"kinds": role.Kinds, "verbs": role.Verbs} {
		prop, ok := rule[field].(map[string]any)
		if !ok {
			t.Fatalf("rule has no %s property", field)
		}
		if prop["minItems"] == nil {
			t.Errorf("%s has no minItems: an empty rule grants nothing and should not validate", field)
		}
		items, _ := prop["items"].(map[string]any)
		enum, _ := items["enum"].([]any)
		if len(enum) != len(want) {
			t.Fatalf("%s enum has %d values, want the %d in the vocabulary", field, len(enum), len(want))
		}
		got := map[string]bool{}
		for _, v := range enum {
			got[v.(string)] = true
		}
		for _, v := range want {
			if !got[v] {
				t.Errorf("%s enum is missing %q", field, v)
			}
		}
	}
}

// ruleSchema digs the rule object out of the generated $defs.
func ruleSchema(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	defs, _ := doc["$defs"].(map[string]any)
	for _, def := range defs {
		obj, ok := def.(map[string]any)
		if !ok {
			continue
		}
		props, ok := obj["properties"].(map[string]any)
		if !ok {
			continue
		}
		if _, hasKinds := props["kinds"]; hasKinds {
			if _, hasVerbs := props["verbs"]; hasVerbs {
				return props
			}
		}
	}
	t.Fatal("no rule definition in the Role schema")
	return nil
}
