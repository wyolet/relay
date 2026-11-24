package control

import (
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/httpapi/openapitest"
)

// The pseudo kinds that delete or change existing rows document blockers on their 409 as the ordinary delete's 409 does: the same field of the same type.
func TestPseudoKinds_DocumentBlockers(t *testing.T) {
	d := mountDeps(t)
	d.PseudoKinds = []string{"all"}
	doc, err := openapitest.Decode(Mount(chi.NewRouter(), d).OpenAPI())
	if err != nil {
		t.Fatal(err)
	}
	schemas, _ := doc["components"].(map[string]any)["schemas"].(map[string]any)
	blockersOf := func(path, method string) any {
		t.Helper()
		ref := responseRef(operation(t, doc, path, method), "409")
		s, _ := schemas[ref[len("#/components/schemas/"):]].(map[string]any)
		props, _ := s["properties"].(map[string]any)
		b, _ := props["blockers"].(map[string]any)
		return b["items"]
	}
	want := blockersOf("/pools/by-id/{id}", "delete")
	if want == nil {
		t.Fatal("the ordinary delete's 409 declares no blockers")
	}
	for _, path := range []string{"/pseudo-agent-removals", "/pseudo-access", "/pseudo-limits"} {
		if got := blockersOf(path, "post"); !sameRef(got, want) {
			t.Errorf("%s: 409 blockers items %v, want %v", path, got, want)
		}
	}
}

// sameRef reports whether two schemas are the same $ref.
func sameRef(a, b any) bool {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	return aok && bok && am["$ref"] != nil && am["$ref"] == bm["$ref"]
}
