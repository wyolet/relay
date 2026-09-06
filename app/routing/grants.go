// PolicyAllows answers "is this Model reachable through this Policy?"
// without picking a binding or a key — the question /v1/models needs to
// answer. Mirrors the allowed-paths logic in Resolve so the two stay in
// sync. Resolve is binding-aware (legacy + DSL + wildcard match against
// specific (provider, model, host) triples); PolicyAllows reduces to
// "is there *any* binding under this policy that would be allowed?"
package routing

import (
	"github.com/wyolet/relay/app/adapters"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
)

// PolicyAllows reports whether m is reachable through pol given snap's
// hostkey coverage. Used to enumerate accessible models for inventory
// endpoints. Single-shot; not optimised for tight loops.
func PolicyAllows(snap *appcatalog.Snapshot, pol *policy.Policy, m *model.Model) bool {
	if pol == nil || m == nil || !m.IsEnabled() || !pol.IsEnabled() {
		return false
	}
	deprecated := isDeprecated(m)
	wildcardGrant := len(pol.Spec.ModelIDs) == 0 && len(pol.Spec.Models) == 0

	for _, hb := range snap.BindingsForModel(m.Meta.ID) {
		if !hb.IsEnabled() {
			continue
		}
		h, ok := snap.Host(hb.Spec.HostID)
		if !ok {
			continue
		}
		// Explicit policies consult the precomputed allow-set; implicit
		// wildcards allow any non-deprecated model (mirrors Resolve).
		var allowed bool
		if wildcardGrant {
			allowed = (!deprecated || pol.Spec.IncludeDeprecated) && !h.Spec.NoAuth
		} else {
			allowed = snap.PolicyAllowsCombo(pol.Meta.ID, m.Meta.ID, hb.Spec.HostID)
		}
		if !allowed {
			continue
		}
		// Same key + tier gate Resolve applies, so a listed model is one the
		// caller can actually reach.
		if len(candidateKeys(snap, pol, m, h)) > 0 {
			return true
		}
	}
	return false
}

// PolicylessAllows reports whether m is reachable by a request that resolved
// no policy — the inventory question matching resolvePolicyless, which is the
// only thing that serves such a request. adapter narrows the answer to
// bindings declaring that wire shape; empty accepts any. userID is the calling
// user, scoping the pool exactly as resolution does.
//
// Mirrors resolvePolicyless step for step: enabled model, not deprecated,
// enabled binding, resolvable host, and a key the D73 pool actually yields.
func PolicylessAllows(snap *appcatalog.Snapshot, m *model.Model, adapter adapters.Name, userID string) bool {
	if m == nil || !m.IsEnabled() || isDeprecated(m) {
		return false
	}
	for _, hb := range snap.BindingsForModel(m.Meta.ID) {
		if !hb.IsEnabled() {
			continue
		}
		if adapter != "" && hb.Spec.Adapter != adapter {
			continue
		}
		h, ok := snap.Host(hb.Spec.HostID)
		if !ok {
			continue
		}
		if len(policylessKeys(snap, m, h, userID)) > 0 {
			return true
		}
	}
	return false
}
