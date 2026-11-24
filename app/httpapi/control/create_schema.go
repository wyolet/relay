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

// specOptionalOnCreate are the kinds whose every spec field is optional, so a create may omit spec. Update keeps spec required: an omitted spec there would reset the stored row, re-enabling a disabled one.
var specOptionalOnCreate = map[string]bool{"team": true, "group": true}

// createRequestBody is the create body for T: a named component, <T>Create,
// with T's shape but metadata.name optional, since create derives the slug
// from displayName, and spec optional for the kinds specOptionalOnCreate
// names. Update and responses keep T's schema, where both are required. Nil
// (huma's default body) when T has no metadata requiring a name.
func createRequestBody[T any](api huma.API, singular string) *huma.RequestBody {
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
		if specOptionalOnCreate[singular] {
			createBody.Required = slices.DeleteFunc(slices.Clone(body.Required), func(f string) bool { return f == "spec" })
		}
		createBody.PrecomputeMessages()
		schemas[name] = &createBody
	}
	return &huma.RequestBody{Content: map[string]*huma.MediaType{"application/json": {Schema: &huma.Schema{Ref: path.Dir(ref) + "/" + name}}}}
}
