package control

import (
	"maps"
	"reflect"
	"slices"

	"github.com/danielgtaylor/huma/v2"
)

// createRequestBody is the create body for T: its shared schema with
// metadata.name optional, since create derives the slug from displayName.
// Update and responses keep the shared schema, where name is required. Nil
// (huma's default body) when T has no metadata object requiring a name.
func createRequestBody[T any](api huma.API) *huma.RequestBody {
	reg := api.OpenAPI().Components.Schemas
	body := reg.SchemaFromRef(reg.Schema(reflect.TypeFor[T](), true, "").Ref)
	if body == nil || body.Properties["metadata"] == nil {
		return nil
	}
	md := body.Properties["metadata"]
	if md.Ref != "" {
		md = reg.SchemaFromRef(md.Ref)
	}
	if md == nil || !slices.Contains(md.Required, "name") {
		return nil
	}
	createMeta := *md
	createMeta.Ref = ""
	createMeta.Description = metadataDescription
	createMeta.Required = slices.DeleteFunc(slices.Clone(md.Required), func(f string) bool { return f == "name" })
	createBody := *body
	createBody.Properties = maps.Clone(body.Properties)
	createBody.Properties["metadata"] = &createMeta
	return &huma.RequestBody{Content: map[string]*huma.MediaType{"application/json": {Schema: &createBody}}}
}
