// PolicyAllows answers "is this Model reachable through this Policy?"
// without picking a binding or a key — the question /v1/models needs to
// answer. Mirrors the allowed-paths logic in Resolve so the two stay in
// sync. Resolve is binding-aware (legacy + DSL + wildcard match against
// specific (provider, model, host) triples); PolicyAllows reduces to
// "is there *any* binding under this policy that would be allowed?"
package routing

import (
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
)

// PolicyAllows reports whether m is reachable through pol given snap's
// hostkey coverage. Used to enumerate accessible models for inventory
// endpoints. Single-shot; not optimised for tight loops.
func PolicyAllows(snap *appcatalog.Snapshot, pol *policy.Policy, m *model.Model) bool {
	if pol == nil || m == nil || !m.IsEnabled() {
		return false
	}
	for _, id := range pol.Spec.ModelIDs {
		if id == m.Meta.ID {
			return true
		}
	}
	deprecated := isDeprecated(m)
	wildcardGrant := len(pol.Spec.ModelIDs) == 0 && len(pol.Spec.Models) == 0

	keyHosts := map[string]struct{}{}
	for _, k := range snap.HostKeysInPolicy(pol.Meta.ID) {
		keyHosts[k.Spec.HostID] = struct{}{}
	}

	for i := range m.Spec.Hosts {
		hb := &m.Spec.Hosts[i]
		if !hb.IsEnabled() {
			continue
		}
		if _, ok := keyHosts[hb.HostID]; !ok {
			continue
		}
		// Explicit policies consult the precomputed allow-set; implicit
		// wildcards allow any non-deprecated model (mirrors Resolve).
		if wildcardGrant {
			if !deprecated || pol.Spec.IncludeDeprecated {
				return true
			}
		} else if snap.PolicyAllowsCombo(pol.Meta.ID, m.Meta.ID, hb.HostID) {
			return true
		}
	}
	return false
}
