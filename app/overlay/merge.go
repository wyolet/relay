package overlay

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/pkg/slug"
)

// Merge semantics are JSON-merge-patch (RFC 7386) with one deviation:
// a small per-kind whitelist of set-like string-array fields merges by
// UNION (template ∪ patch, deduped on the field's own equality) instead
// of wholesale replace. Everything else follows merge-patch: null
// deletes a field, objects recurse, arrays and scalars replace.
//
// The whitelist is a deliberate hardcoded table, not an annotation
// system: every future field gets a conscious decision. Ordered arrays
// (position is semantics) and keyed-object lists (would need per-item
// merge keys — the strategic-merge swamp) stay replace; union fields
// can only ADD relative to the template — removal requires deleting the
// field from the patch or escalating to a direct edit.

// unionKeyers maps kind → top-level spec field → dedup keyer for
// union-merged set-like string arrays.
var unionKeyers = map[string]map[string]func(string) string{
	KindModel: {
		"aliases": slug.From, // dedup on the normalized form the resolver matches on
		"tags":    strings.ToLower,
	},
}

// MergeSpec applies patch onto templateSpec (both JSON objects) per the
// kind's strategy and returns the effective spec JSON.
func MergeSpec(kind string, templateSpec, patch []byte) ([]byte, error) {
	var tmpl map[string]any
	if err := json.Unmarshal(templateSpec, &tmpl); err != nil {
		return nil, fmt.Errorf("overlay merge: template spec: %w", err)
	}
	var p map[string]any
	if err := json.Unmarshal(patch, &p); err != nil {
		return nil, fmt.Errorf("overlay merge: patch: %w", err)
	}
	if tmpl == nil {
		tmpl = map[string]any{}
	}
	mergeInto(tmpl, p, unionKeyers[kind])
	out, err := json.Marshal(tmpl)
	if err != nil {
		return nil, fmt.Errorf("overlay merge: marshal effective: %w", err)
	}
	return out, nil
}

// mergeInto applies patch onto dst in place. unionFields applies to
// top-level keys only (set-like arrays live at the spec's top level).
func mergeInto(dst, patch map[string]any, unionFields map[string]func(string) string) {
	for k, pv := range patch {
		if pv == nil {
			delete(dst, k)
			continue
		}
		if keyer, isUnion := unionFields[k]; isUnion {
			if merged, ok := unionStrings(dst[k], pv, keyer); ok {
				dst[k] = merged
				continue
			}
			// Not two string arrays (template missing, or wrong shape) —
			// fall through to replace; post-merge Validate catches junk.
		}
		if dm, dok := dst[k].(map[string]any); dok {
			if pm, pok := pv.(map[string]any); pok {
				mergeInto(dm, pm, nil) // union whitelist is top-level only
				continue
			}
		}
		dst[k] = pv
	}
}

// unionStrings merges two JSON string arrays, template entries first,
// deduping on keyer. ok=false when either side isn't a string array.
func unionStrings(tmplVal, patchVal any, keyer func(string) string) ([]any, bool) {
	t, ok := toStrings(tmplVal)
	if !ok && tmplVal != nil {
		return nil, false
	}
	p, ok := toStrings(patchVal)
	if !ok {
		return nil, false
	}
	seen := make(map[string]struct{}, len(t)+len(p))
	out := make([]any, 0, len(t)+len(p))
	for _, s := range append(t, p...) {
		key := keyer(s)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, s)
	}
	return out, true
}

func toStrings(v any) ([]string, bool) {
	if v == nil {
		return nil, true
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, len(arr))
	for i, e := range arr {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out[i] = s
	}
	return out, true
}

// EffectiveModel merges o onto the template model and validates the
// result. The returned model shares the template's Metadata (identity is
// never patchable); only Spec differs. An error means the merge produced
// an invalid effective row — callers QUARANTINE the overlay (serve the
// pristine template, log loudly) rather than failing the snapshot.
func EffectiveModel(template *model.Model, o *Overlay) (*model.Model, error) {
	if template == nil {
		return nil, fmt.Errorf("overlay: template model is nil")
	}
	if o == nil {
		return template, nil
	}
	tmplSpec, err := json.Marshal(template.Spec)
	if err != nil {
		return nil, fmt.Errorf("overlay: marshal template spec: %w", err)
	}
	merged, err := MergeSpec(KindModel, tmplSpec, o.Patch)
	if err != nil {
		return nil, err
	}
	var spec model.Spec
	if err := json.Unmarshal(merged, &spec); err != nil {
		return nil, fmt.Errorf("overlay: effective spec does not decode: %w", err)
	}
	eff := *template
	eff.Spec = spec
	if err := eff.Validate(); err != nil {
		return nil, fmt.Errorf("overlay: effective row invalid: %w", err)
	}
	return &eff, nil
}
