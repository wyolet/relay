package settings

import (
	"github.com/danielgtaylor/huma/v2"
)

// SectionName is a typed string whose OpenAPI schema is a string enum
// of every registered section. Use it in handler request/response
// types instead of bare `string` so the generated spec carries the
// closed set of valid section keys.
type SectionName string

// Schema implements huma.SchemaProvider — huma calls this when
// registering any type that embeds SectionName, snapshotting the
// current registry into the OpenAPI components.
func (SectionName) Schema(_ huma.Registry) *huma.Schema {
	names := Names()
	values := make([]any, len(names))
	for i, n := range names {
		values[i] = n
	}
	return &huma.Schema{
		Type:        huma.TypeString,
		Enum:        values,
		Description: "One of the relay's registered settings section keys.",
	}
}

var _ huma.SchemaProvider = SectionName("")
