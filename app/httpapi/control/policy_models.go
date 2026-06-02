// Policy model-grant normalization + enabled-resolution guard.
//
// At policy create/update the catalog-ref strings in Spec.Models (and each
// RLBinding's Models) are:
//
//  1. slugified to canonical form — operators may paste real-world names
//     ("openai/GPT-4o", "anthropic/claude-3.5") and they're rewritten to the
//     stored slug form ("openai/gpt-4o", "anthropic/claude-3-5"). This keeps
//     PG, the data-plane snapshot, and the picker agreeing on one string.
//  2. resolved against the catalog and required to match at least one
//     *enabled* binding (model enabled, host enabled, binding enabled).
//     Host-only "@host" refs only require the host to exist + be enabled —
//     they grant every present and future binding on that host.
//
// Refs that resolve to nothing enabled are rejected with 400. This is the
// cross-entity check the per-row policy.Validate() (grammar only, no catalog
// access) can't perform. Host-key / relay-key existence is deliberately NOT
// checked here — the inference path handles those at request time.
package control

import (
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/modelref"
	"github.com/wyolet/relay/app/policy"
)

func guardPolicyModels(d Deps) mutationGuard[policy.Policy] {
	return func(action string, _, incoming *policy.Policy) error {
		if action == "delete" || incoming == nil {
			return nil
		}
		if len(incoming.Spec.Models) == 0 && len(incoming.Spec.RLBindings) == 0 {
			return nil
		}
		idx, err := loadResolveIndex(d)
		if err != nil {
			return huma.Error500InternalServerError("load catalog: " + err.Error())
		}

		name := incoming.Meta.Name
		if len(incoming.Spec.Models) > 0 {
			norm, err := normalizePolicyRefs(idx, incoming.Spec.Models, name, "models")
			if err != nil {
				return err
			}
			incoming.Spec.Models = norm
		}
		for i := range incoming.Spec.RLBindings {
			norm, err := normalizePolicyRefs(idx, incoming.Spec.RLBindings[i].Models, name, "rlBindings")
			if err != nil {
				return err
			}
			incoming.Spec.RLBindings[i].Models = norm
		}
		return nil
	}
}

// normalizePolicyRefs slugifies each ref to canonical form, drops post-slug
// duplicates, and rejects any ref that doesn't resolve to an enabled binding.
func normalizePolicyRefs(idx *resolveIndex, refs []string, polName, field string) ([]string, error) {
	if len(refs) == 0 {
		return refs, nil
	}
	out := make([]string, 0, len(refs))
	seen := make(map[string]struct{}, len(refs))
	for _, raw := range refs {
		ref, err := modelref.Parse(raw)
		if err != nil {
			return nil, huma.Error400BadRequest(fmt.Sprintf("policy %q: %s: %v", polName, field, err))
		}
		canonical := canonicalRef(ref)
		if _, dup := seen[canonical]; dup {
			continue
		}
		seen[canonical] = struct{}{}
		if !refResolvesEnabled(idx, ref) {
			return nil, huma.Error400BadRequest(fmt.Sprintf(
				"policy %q: %s ref %q matches no enabled model or host in the catalog",
				polName, field, canonical))
		}
		out = append(out, canonical)
	}
	return out, nil
}

// canonicalRef renders a parsed Ref back to its shortest slug form. The Ref's
// segments are already slug-normalized by modelref.Parse.
func canonicalRef(r modelref.Ref) string {
	prov, mdl, hst := "", "", ""
	if !r.ProviderWildcard {
		prov = r.Provider
	}
	if !r.ModelWildcard {
		mdl = r.Model
	}
	if !r.HostWildcard {
		hst = r.Host
	}
	s, err := modelref.Format(prov, mdl, hst)
	if err != nil {
		return r.Raw
	}
	return s
}

// refResolvesEnabled reports whether ref matches at least one binding whose
// model, host, and binding are all enabled. A host-only ref ("@host") needs
// only an existing, enabled host (it grants future bindings too).
func refResolvesEnabled(idx *resolveIndex, ref modelref.Ref) bool {
	if ref.ProviderWildcard {
		for _, h := range idx.hostsByID {
			if h.Meta.Name == ref.Host && h.IsEnabled() {
				return true
			}
		}
		return false
	}
	prov, ok := idx.providersByName[ref.Provider]
	if !ok {
		return false
	}
	for _, m := range idx.modelsByProvider[prov.Meta.ID] {
		if !m.IsEnabled() {
			continue
		}
		if !ref.ModelWildcard && m.Meta.Name != ref.Model {
			continue
		}
		for _, hb := range idx.snap.BindingsForModel(m.Meta.ID) {
			if !hb.IsEnabled() {
				continue
			}
			h, ok := idx.hostsByID[hb.Spec.HostID]
			if !ok || !h.IsEnabled() {
				continue
			}
			if ref.Matches(prov.Meta.Name, m.Meta.Name, h.Meta.Name) {
				return true
			}
		}
	}
	return false
}
