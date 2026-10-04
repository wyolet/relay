package manifest

import (
	"bytes"
	"fmt"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// SchemaVersion is the path segment the JSON Schemas are served under —
// the trailing element of APIVersion, so the two can never drift.
var SchemaVersion = path.Base(APIVersion)

// Render writes docs as a multi-document YAML bundle. Each document is
// preceded by the editor `$schema` directive pointing at the relay's own
// schema endpoint; schemaBase is the deployment's public origin, or empty
// for a relative (same-origin) reference.
//
// This is the package's only YAML writer — everything else parses.
func Render(docs []Document, schemaBase string) ([]byte, error) {
	var buf bytes.Buffer
	first := true
	for _, d := range docs {
		body := d.Payload()
		if body == nil {
			continue
		}
		if !first {
			buf.WriteString("---\n")
		}
		first = false
		buf.WriteString(SchemaRef(d.Kind(), schemaBase))
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(body); err != nil {
			return nil, fmt.Errorf("manifest: render %s: %w", d.Kind(), err)
		}
		if err := enc.Close(); err != nil {
			return nil, fmt.Errorf("manifest: render %s: %w", d.Kind(), err)
		}
	}
	return buf.Bytes(), nil
}

// SchemaRef is the yaml-language-server directive line for one kind.
func SchemaRef(kind, base string) string {
	return fmt.Sprintf("# yaml-language-server: $schema=%s/api/schemas/%s/%s.schema.json\n",
		strings.TrimSuffix(base, "/"), SchemaVersion, kind)
}

// Meta returns a pointer to the document's metadata block, or nil for an
// empty document. Render-side callers use it to rewrite owner references
// from ids to names.
func (d Document) Meta() *WireMeta {
	switch v := d.Payload().(type) {
	case *ProviderDTO:
		return &v.Metadata
	case *HostDTO:
		return &v.Metadata
	case *ModelDTO:
		return &v.Metadata
	case *HostKeyDTO:
		return &v.Metadata
	case *PolicyDTO:
		return &v.Metadata
	case *RateLimitDTO:
		return &v.Metadata
	case *KeyDTO:
		return &v.Metadata
	case *PricingDTO:
		return &v.Metadata
	case *HostBindingDTO:
		return &v.Metadata
	case *TeamDTO:
		return &v.Metadata
	case *ProjectDTO:
		return &v.Metadata
	case *ServiceAccountDTO:
		return &v.Metadata
	case *GroupDTO:
		return &v.Metadata
	case *RoleDTO:
		return &v.Metadata
	case *RoleBindingDTO:
		return &v.Metadata
	case *PolicyBindingDTO:
		return &v.Metadata
	case *OverlayDTO:
		return &v.Metadata
	}
	return nil
}

// Payload returns the concrete DTO carried by the document, or nil when the
// document is empty or of a kind that is never rendered (Setting's spec is a
// raw node the settings store owns).
func (d Document) Payload() any {
	switch {
	case d.Provider != nil:
		return d.Provider
	case d.Host != nil:
		return d.Host
	case d.Model != nil:
		return d.Model
	case d.HostKey != nil:
		return d.HostKey
	case d.Policy != nil:
		return d.Policy
	case d.RateLimit != nil:
		return d.RateLimit
	case d.Key != nil:
		return d.Key
	case d.Pricing != nil:
		return d.Pricing
	case d.HostBinding != nil:
		return d.HostBinding
	case d.Team != nil:
		return d.Team
	case d.Project != nil:
		return d.Project
	case d.ServiceAccount != nil:
		return d.ServiceAccount
	case d.Group != nil:
		return d.Group
	case d.Role != nil:
		return d.Role
	case d.RoleBinding != nil:
		return d.RoleBinding
	case d.PolicyBinding != nil:
		return d.PolicyBinding
	case d.Overlay != nil:
		return d.Overlay
	default:
		return nil
	}
}
