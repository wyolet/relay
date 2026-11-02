package control

import (
	"slices"
	"testing"
)

// The holder's and the key principal's name is documented read-only and never required, so a generated client neither must nor may send it.
func TestControlOpenAPINamesHoldersReadOnly(t *testing.T) {
	doc := controlDoc(t)
	for _, schema := range []string{"AccountHolder", "KeyPrincipal"} {
		props := schemaProps(t, doc, schema)
		name, _ := props["name"].(map[string]any)
		if name == nil || name["readOnly"] != true {
			t.Errorf("%s.name = %v, want a read-only property", schema, name)
		}
		comps, _ := doc["components"].(map[string]any)
		s, _ := comps["schemas"].(map[string]any)[schema].(map[string]any)
		req, _ := s["required"].([]any)
		if slices.Contains(req, any("name")) {
			t.Errorf("%s requires name", schema)
		}
	}
}
