// The JSONB shapes are fixed: field names, order and omitempty behaviour are pinned by golden tests, because a product sharing these tables reads and writes the same documents. A spec field the module does not know is kept in Attributes and written back as it came.

package postgres

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/directory"
)

// metadataDoc is the metadata column. ID, name and display name have columns of their own and are not repeated here.
type metadataDoc struct {
	Description string            `json:"description,omitempty"`
	Owner       ownerDoc          `json:"owner,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Dirty       bool              `json:"dirty,omitempty"`
}

type ownerDoc struct {
	Kind string `json:"kind,omitempty"`
	ID   string `json:"id,omitempty"`
}

func encodeMetadata(m directory.Metadata) ([]byte, error) {
	return json.Marshal(metadataDoc{
		Description: m.Description,
		Owner:       ownerDoc{Kind: m.Owner.Kind, ID: m.Owner.ID},
		Labels:      m.Labels,
		Annotations: m.Annotations,
		Dirty:       m.Dirty,
	})
}

// decodeMetadata rebuilds Metadata from the row's columns and its metadata document.
func decodeMetadata(id, name, displayName string, raw []byte, created, updated pgtype.Timestamptz, version int64) (directory.Metadata, error) {
	m := directory.Metadata{
		ID: id, Name: name, DisplayName: displayName,
		CreatedAt: created.Time, UpdatedAt: updated.Time,
		ResourceVersion: directory.FormatResourceVersion(version),
	}
	if len(raw) == 0 {
		return m, nil
	}
	var d metadataDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		return m, fmt.Errorf("metadata: %w", err)
	}
	m.Description = d.Description
	m.Owner = authz.Owner{Kind: d.Owner.Kind, ID: d.Owner.ID}
	m.Labels, m.Annotations, m.Dirty = d.Labels, d.Annotations, d.Dirty
	return m, nil
}

// encodeSpec marshals doc, the fields the module owns, then appends attrs in key order. An attribute named like a field the kind owns is refused rather than silently shadowed.
func encodeSpec(doc any, attrs map[string]json.RawMessage, owned []string) ([]byte, error) {
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	if len(attrs) == 0 {
		return b, nil
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		if slices.Contains(owned, k) {
			return nil, fmt.Errorf("%w: spec attribute %q is a field the module owns", directory.ErrInvalid, k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	buf.Write(b[:len(b)-1])
	for _, k := range keys {
		if buf.Len() > 1 {
			buf.WriteByte(',')
		}
		name, _ := json.Marshal(k)
		buf.Write(name)
		buf.WriteByte(':')
		if err := json.Compact(&buf, attrs[k]); err != nil {
			return nil, fmt.Errorf("%w: spec attribute %q is not JSON: %v", directory.ErrInvalid, k, err)
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// decodeSpec unmarshals raw into doc and returns every key outside owned as an attribute, compacted.
func decodeSpec(raw []byte, doc any, owned []string) (map[string]json.RawMessage, error) {
	if err := json.Unmarshal(raw, doc); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	var attrs map[string]json.RawMessage
	for k, v := range all {
		if slices.Contains(owned, k) {
			continue
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, v); err != nil {
			return nil, fmt.Errorf("spec attribute %q: %w", k, err)
		}
		if attrs == nil {
			attrs = map[string]json.RawMessage{}
		}
		attrs[k] = buf.Bytes()
	}
	return attrs, nil
}

// The spec documents, one per kind, each with the keys it owns.

type enabledDoc struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// groupSpecKeys includes memberIds: the members live in group_members, and a document carrying the key is read as owning it, not as an attribute.
var groupSpecKeys = []string{"memberIds", "enabled"}

var teamSpecKeys = []string{"enabled"}

type projectSpecDoc struct {
	TeamID  string `json:"teamId"`
	Enabled *bool  `json:"enabled,omitempty"`
}

var projectSpecKeys = []string{"teamId", "enabled"}

type serviceAccountSpecDoc struct {
	ProjectID string `json:"projectId"`
	Enabled   *bool  `json:"enabled,omitempty"`
}

var serviceAccountSpecKeys = []string{"projectId", "enabled"}

type roleSpecDoc struct {
	Rules   []ruleDoc `json:"rules"`
	Enabled *bool     `json:"enabled,omitempty"`
}

type ruleDoc struct {
	Kinds []string `json:"kinds"`
	Verbs []string `json:"verbs"`
}

var roleSpecKeys = []string{"rules", "enabled"}

// bindingSpecDoc writes the role, scope and subjects as empty placeholders: their values live in columns and the subjects junction, and the placeholders keep the document's shape fixed.
type bindingSpecDoc struct {
	RoleID   string            `json:"roleId"`
	Scope    ownerDoc          `json:"scope"`
	Subjects []json.RawMessage `json:"subjects"`
	Enabled  *bool             `json:"enabled,omitempty"`
}

var bindingSpecKeys = []string{"roleId", "scope", "subjects", "enabled"}

// encodeIdentity renders an identity as the oidc_subject column holds it.
func encodeIdentity(i directory.Identity) string {
	if i.IsZero() {
		return ""
	}
	return i.Issuer + directory.IdentitySeparator + i.Subject
}

// decodeIdentity splits at the first separator; the issuer may not contain one, the subject may.
func decodeIdentity(s string) directory.Identity {
	if s == "" {
		return directory.Identity{}
	}
	issuer, subject, ok := strings.Cut(s, directory.IdentitySeparator)
	if !ok {
		return directory.Identity{Subject: s}
	}
	return directory.Identity{Issuer: issuer, Subject: subject}
}
