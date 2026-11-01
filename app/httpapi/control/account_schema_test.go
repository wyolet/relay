package control

import (
	"testing"

	"github.com/go-chi/chi/v5"
)

// An account's orgId is what detecting a key's account matches on, so the document says what it holds.
func TestOpenAPIDescribesAccountOrgID(t *testing.T) {
	doc := Mount(chi.NewRouter(), mountDeps(t)).OpenAPI()
	spec := doc.Components.Schemas.Map()["AccountSpec"]
	if spec == nil {
		t.Fatal("no AccountSpec schema")
	}
	org := spec.Properties["orgId"]
	if org == nil || org.Description == "" {
		t.Fatalf("AccountSpec.orgId = %+v, want a described property", org)
	}
}
