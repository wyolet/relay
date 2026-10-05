package control

import (
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// Every entity update declares the 409 a stale resourceVersion answers with,
// and Metadata carries the field clients send back, so a generated client
// can handle both without reading prose.
func TestOpenAPIDeclaresResourceVersionConflicts(t *testing.T) {
	api := Mount(chi.NewRouter(), mountDeps(t))
	doc := api.OpenAPI()

	updates := 0
	for path, item := range doc.Paths {
		op := item.Put
		// Users carry no Metadata: their update is a partial patch of the
		// fields sent, outside this contract.
		if op == nil || !strings.HasPrefix(op.OperationID, "update_") || !strings.Contains(path, "/by-id/") ||
			op.OperationID == "update_user" {
			continue
		}
		updates++
		if _, ok := op.Responses["409"]; !ok {
			t.Errorf("%s (%s) does not declare 409", op.OperationID, path)
		}
	}
	if updates < 16 {
		t.Fatalf("found %d entity update operations, want at least 16", updates)
	}

	md := doc.Components.Schemas.Map()["Metadata"]
	if md == nil {
		t.Fatal("no Metadata schema")
	}
	if _, ok := md.Properties["resourceVersion"]; !ok {
		t.Fatal("Metadata schema has no resourceVersion")
	}
}
