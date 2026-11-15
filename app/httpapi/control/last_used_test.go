package control

import (
	"slices"
	"testing"
)

// lastUsedAt is server-written status: read-only, never required, and only under status.
func TestControlOpenAPILastUsedUnderStatus(t *testing.T) {
	doc := controlDoc(t)
	comps, _ := doc["components"].(map[string]any)
	schemas, _ := comps["schemas"].(map[string]any)
	for kind, status := range map[string]string{"Key": "KeyObservedStatus", "HostKey": "HostKeyStatus"} {
		props := schemaProps(t, doc, kind)
		if _, ok := props["lastUsedAt"]; ok {
			t.Errorf("%s has a top-level lastUsedAt", kind)
		}
		st, _ := props["status"].(map[string]any)
		if st == nil || st["readOnly"] != true {
			t.Errorf("%s.status = %v, want read-only", kind, st)
		}
		p, _ := schemaProps(t, doc, status)["lastUsedAt"].(map[string]any)
		if p == nil || p["format"] != "date-time" {
			t.Errorf("%s.lastUsedAt = %v, want a date-time", status, p)
		}
		for _, name := range []string{kind, status} {
			s, _ := schemas[name].(map[string]any)
			req, _ := s["required"].([]any)
			if slices.Contains(req, any("status")) || slices.Contains(req, any("lastUsedAt")) {
				t.Errorf("%s requires %v", name, req)
			}
		}
	}
}
