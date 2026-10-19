package control

import (
	"regexp"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

// keyMaterialName matches property names that would carry key bytes or a decrypted value.
var keyMaterialName = regexp.MustCompile(`(?i)^wrapped|material|plain|cipher|secret|value|bytes|^dek$|^key$`)

// No /secret-keys response schema has a field that could carry key material: every property is listed, and none is binary or named like key bytes.
func TestSecretKeysResponsesCarryNoKeyMaterial(t *testing.T) {
	s := newSecretKeysHarness(t, &fakeSecretKeys{})
	doc := s.api.OpenAPI()
	reg := doc.Components.Schemas
	ops := 0
	for path, item := range doc.Paths {
		if !strings.HasPrefix(path, "/secret-keys") {
			continue
		}
		for _, op := range []*huma.Operation{item.Get, item.Post} {
			if op == nil {
				continue
			}
			ops++
			resp := op.Responses["200"]
			if resp == nil || resp.Content["application/json"] == nil {
				t.Fatalf("%s has no JSON 200 response", op.OperationID)
			}
			props := map[string]bool{}
			collectProps(t, reg, resp.Content["application/json"].Schema, props, 0)
			if len(props) == 0 {
				t.Fatalf("%s: no properties found", op.OperationID)
			}
			for name := range props {
				if keyMaterialName.MatchString(name) {
					t.Errorf("%s returns a field named %q", op.OperationID, name)
				}
			}
		}
	}
	if ops != 4 {
		t.Fatalf("found %d /secret-keys operations, want 4", ops)
	}
}

func collectProps(t *testing.T, reg huma.Registry, s *huma.Schema, into map[string]bool, depth int) {
	t.Helper()
	if s == nil || depth > 8 {
		return
	}
	if s.Ref != "" {
		s = reg.SchemaFromRef(s.Ref)
		if s == nil {
			t.Fatal("unresolved schema ref")
		}
	}
	if s.Format == "byte" || s.Format == "binary" || s.ContentEncoding != "" {
		t.Errorf("binary field in a /secret-keys response: %+v", s)
	}
	if s.AdditionalProperties != nil && s.AdditionalProperties != false {
		t.Errorf("open object in a /secret-keys response")
	}
	for name, p := range s.Properties {
		into[name] = true
		collectProps(t, reg, p, into, depth+1)
	}
	collectProps(t, reg, s.Items, into, depth+1)
}
