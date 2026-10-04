package apply

import (
	"bytes"
	"encoding/json"
	"sort"

	"github.com/wyolet/relay/app/meta"
)

// rowView is the subset of a row a manifest can author. Everything else —
// ids, timestamps, the dirty flag, derived read-only fields (host status,
// host-key policy back-refs) — is server-owned and would report a change on
// every run, so it never reaches the diff.
type rowView struct {
	Metadata metaView        `json:"metadata"`
	Spec     json.RawMessage `json:"spec,omitempty"`
}

type metaView struct {
	DisplayName string            `json:"displayName,omitempty"`
	Description string            `json:"description,omitempty"`
	Owner       meta.Owner        `json:"owner,omitzero"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// viewOf projects a domain row onto the authorable subset. row is marshalled
// once to lift its `spec` verbatim, so almost no kind-specific code is
// needed — the one exception is dropped by stripSpecFields.
func viewOf(kind string, row any, m *meta.Metadata) rowView {
	v := rowView{Metadata: metaView{
		DisplayName: m.DisplayName,
		Description: m.Description,
		Owner:       m.Owner,
		Labels:      m.Labels,
		Annotations: m.Annotations,
	}}
	raw, err := json.Marshal(row)
	if err != nil {
		return v
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return v
	}
	v.Spec = stripSpecFields(kind, top["spec"])
	return v
}

// stripSpecFields drops spec fields that can never compare equal across the
// two sides of a diff. A stored HostKey holds ciphertext (or nothing, for an
// env ref) where the manifest holds the plaintext it declared, so leaving
// `value` in would report a change on every single run.
func stripSpecFields(kind string, spec json.RawMessage) json.RawMessage {
	if kind != "HostKey" || len(spec) == 0 || spec[0] != '{' {
		return spec
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(spec, &fields) != nil {
		return spec
	}
	if _, ok := fields["value"]; !ok {
		return spec
	}
	delete(fields, "value")
	out, err := json.Marshal(fields)
	if err != nil {
		return spec
	}
	return out
}

// changedFields lists the authorable JSON paths that differ between the
// stored row and the one the manifest declares, one level under each
// top-level object ("metadata.displayName", "spec.enabled") — the same path
// vocabulary an audit row's change.fields uses. (Not shared with app/audit:
// that package reaches the catalog for its Authorizer wrapper, and the boot
// seed sits below it.)
func changedFields(existing, incoming rowView) []string {
	a, b := objectOf(existing), objectOf(incoming)
	seen := map[string]bool{}
	var out []string
	for _, top := range unionKeys(a, b) {
		subA, subB := nestedObject(a[top]), nestedObject(b[top])
		if subA == nil && subB == nil {
			if !bytes.Equal(a[top], b[top]) && !seen[top] {
				seen[top] = true
				out = append(out, top)
			}
			continue
		}
		for _, k := range unionKeys(subA, subB) {
			if bytes.Equal(subA[k], subB[k]) {
				continue
			}
			p := top + "." + k
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

func objectOf(v any) map[string]json.RawMessage {
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

func nestedObject(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 || raw[0] != '{' {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
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
