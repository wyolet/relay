package manifest

import (
	"bytes"
	"encoding/json"
	"sort"
)

// ChangedFields lists the document paths, one level under metadata and spec, at which two renderings of a row differ. Ids, owners and timestamps are the store's, so they never count as a change.
func ChangedFields(before, after *Document) []string {
	a, b := view(before), view(after)
	var out []string
	for _, top := range []string{"metadata", "spec"} {
		for _, k := range unionKeys(a[top], b[top]) {
			if !bytes.Equal(a[top][k], b[top][k]) {
				out = append(out, top+"."+k)
			}
		}
	}
	return out
}

func view(d *Document) map[string]map[string]json.RawMessage {
	meta := struct {
		DisplayName string            `json:"displayName,omitempty"`
		Description string            `json:"description,omitempty"`
		Labels      map[string]string `json:"labels,omitempty"`
		Annotations map[string]string `json:"annotations,omitempty"`
	}{d.Metadata.DisplayName, d.Metadata.Description, d.Metadata.Labels, d.Metadata.Annotations}
	spec := object(d.spec())
	for name, v := range d.Fields {
		if spec == nil {
			spec = map[string]json.RawMessage{}
		}
		spec[name] = canonical(v)
	}
	return map[string]map[string]json.RawMessage{"metadata": object(meta), "spec": spec}
}

// object marshals v and splits its top level into fields. Marshalling a struct of plain fields cannot fail; a failure leaves the object empty.
func object(v any) map[string]json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

// canonical re-marshals raw with sorted keys and no whitespace, so a product field as written and as a store (Postgres jsonb reorders keys) returns it compare equal.
func canonical(raw json.RawMessage) json.RawMessage {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}

func unionKeys(a, b map[string]json.RawMessage) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range []map[string]json.RawMessage{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}
