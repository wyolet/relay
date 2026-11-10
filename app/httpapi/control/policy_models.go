// Policy model-grant normalization + enabled-resolution guard.
//
// At policy create/update the catalog-ref strings in Spec.Models (and each
// rate-limit link's Models) are:
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
// access) can't perform. The linked rows are checked by refcheck.

package control

import (
	"context"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/modelref"
)

func guardPolicyModels(d Deps) mutationGuard[policy.Policy] {
	return func(ctx context.Context, action string, existing, incoming *policy.Policy) error {
		if action == "delete" || incoming == nil {
			return nil
		}
		if err := refs(d).Policy(ctx, incoming); err != nil {
			return refErr(err)
		}
		if action == "update" && existing.Meta.Owner != incoming.Meta.Owner {
			if err := refusePolicyMove(ctx, d, incoming); err != nil {
				return err
			}
		}
		if len(incoming.Spec.Models) == 0 && len(incoming.Spec.RateLimits) == 0 {
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
		for i := range incoming.Spec.RateLimits {
			if len(incoming.Spec.RateLimits[i].Models) == 0 {
				continue
			}
			norm, err := normalizePolicyRefs(idx, incoming.Spec.RateLimits[i].Models, name, fmt.Sprintf("rateLimits[%d].models", i))
			if err != nil {
				return err
			}
			incoming.Spec.RateLimits[i].Models = norm
		}
		return nil
	}
}

// normalizePolicyRefs canonicalises each ref exactly as apply does (one
// shared function, or a document and an API write of the same grant would
// store different strings) and rejects any that doesn't resolve to an
// enabled binding — the catalog check apply cannot make.
func normalizePolicyRefs(idx *resolveIndex, refs []string, polName, field string) ([]string, error) {
	canonical, err := policy.CanonicalizeModelRefs(refs)
	if err != nil {
		return nil, huma.Error400BadRequest(fmt.Sprintf("policy %q: %s: %v", polName, field, err))
	}
	for _, c := range canonical {
		ref, err := modelref.Parse(c)
		if err != nil {
			return nil, huma.Error400BadRequest(fmt.Sprintf("policy %q: %s: %v", polName, field, err))
		}
		if !refResolvesEnabled(idx, ref) {
			return nil, huma.Error400BadRequest(fmt.Sprintf(
				"policy %q: %s ref %q matches no enabled model or host in the catalog",
				polName, field, c))
		}
	}
	return canonical, nil
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
