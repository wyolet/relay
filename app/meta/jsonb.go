package meta

import (
	"encoding/json"
	"fmt"
)

// MarshalJSONB renders the JSONB-stored portion of a Metadata. ID, Name, and
// DisplayName live in their own SQL columns, so they are intentionally
// excluded from the JSONB blob to avoid duplication.
func MarshalJSONB(m Metadata) ([]byte, error) {
	return json.Marshal(jsonbDoc{
		Description: m.Description,
		Owner:       m.Owner,
		Labels:      m.Labels,
	})
}

// UnmarshalJSONB reconstructs a Metadata from its column values plus the
// JSONB blob. raw may be empty (legacy rows or freshly-stamped placeholders).
func UnmarshalJSONB(id, name, displayName string, raw []byte) (Metadata, error) {
	m := Metadata{ID: id, Name: name, DisplayName: displayName}
	if len(raw) == 0 {
		return m, nil
	}
	var d jsonbDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		return m, fmt.Errorf("metadata: %w", err)
	}
	m.Description = d.Description
	m.Owner = d.Owner
	m.Labels = d.Labels
	return m, nil
}

// jsonbDoc is the on-disk JSONB shape. Internal — entity packages go through
// MarshalJSONB/UnmarshalJSONB.
type jsonbDoc struct {
	Description string            `json:"description,omitempty"`
	Owner       Owner             `json:"owner,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}
