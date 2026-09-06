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
// access) can't perform. Host-key / key existence is deliberately NOT
// checked here — the inference path handles those at request time.
package control

import (
	"context"
	"fmt"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/modelref"
	"github.com/wyolet/relay/app/policy"
)

func guardPolicyModels(d Deps) mutationGuard[policy.Policy] {
	return func(ctx context.Context, action string, existing, incoming *policy.Policy) error {
		if action == "delete" {
			return guardPolicyDeleteTier(ctx, d, existing)
		}
		if incoming == nil {
			return nil
		}
		if err := checkHostKeyRefsVisible(ctx, d, incoming.Spec.HostKeyIDs, incoming.Meta.Owner); err != nil {
			return err
		}
		if err := checkRateLimitRefVisible(ctx, d, incoming.Spec.RateLimitID, incoming.Meta.Owner); err != nil {
			return err
		}
		for _, b := range incoming.Spec.RLBindings {
			if err := checkRateLimitRefVisible(ctx, d, b.RateLimitID, incoming.Meta.Owner); err != nil {
				return err
			}
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

// guardPolicyDeleteTier refuses to delete a policy that host keys use as
// their tier policy: the delete cascade would clear their spec.policyId and
// leave every one of them invalid, failing its next write and vanishing from
// the snapshot. Refusing names the rows the operator has to reattach first.
func guardPolicyDeleteTier(ctx context.Context, d Deps, existing *policy.Policy) error {
	if existing == nil || d.Stores == nil || d.Stores.HostKey == nil {
		return nil
	}
	names, err := policy.HostKeysUsingPolicy(ctx, detachStores(d), existing.Meta.ID)
	if err != nil {
		return huma.Error500InternalServerError(err.Error())
	}
	if len(names) > 0 {
		return huma.Error409Conflict(fmt.Sprintf(
			"policy %q is the tier policy of host key(s) %s: reattach them before deleting it",
			existing.Meta.Name, strings.Join(names, ", ")))
	}
	return nil
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
