package control

import (
	"maps"
	"path"
	"reflect"
	"slices"

	"github.com/danielgtaylor/huma/v2"
)

// createMetadataSchema names Metadata with name optional, shared by every create body.
const createMetadataSchema = "MetadataCreate"

// createRequestBody is the create body for T: a named component, <T>Create,
// with T's shape but metadata.name optional, since create derives the slug
// from displayName. Update and responses keep T's schema, where name is
// required. Nil (huma's default body) when T has no metadata requiring a name.
func createRequestBody[T any](api huma.API) *huma.RequestBody {
	reg := api.OpenAPI().Components.Schemas
	ref := reg.Schema(reflect.TypeFor[T](), true, "").Ref
	body := reg.SchemaFromRef(ref)
	if body == nil || body.Properties["metadata"] == nil || body.Properties["metadata"].Ref == "" {
		return nil
	}
	md := reg.SchemaFromRef(body.Properties["metadata"].Ref)
	if md == nil || !slices.Contains(md.Required, "name") {
		return nil
	}
	// Registered by name: the UI's client is generated from these components.
	schemas := reg.Map()
	if schemas[createMetadataSchema] == nil {
		createMeta := *md
		createMeta.Properties = maps.Clone(md.Properties)
		createMeta.Required = slices.DeleteFunc(slices.Clone(md.Required), func(f string) bool { return f == "name" })
		createMeta.PrecomputeMessages()
		schemas[createMetadataSchema] = &createMeta
	}
	// Kinds sharing a Go type share its create schema.
	name := path.Base(ref) + "Create"
	if schemas[name] == nil {
		createBody := *body
		createBody.Properties = maps.Clone(body.Properties)
		createBody.Properties["metadata"] = &huma.Schema{Ref: path.Dir(ref) + "/" + createMetadataSchema, Description: metadataDescription}
		createBody.PrecomputeMessages()
		schemas[name] = &createBody
	}
	return &huma.RequestBody{Content: map[string]*huma.MediaType{"application/json": {Schema: &huma.Schema{Ref: path.Dir(ref) + "/" + name}}}}
}
