// PolicyAllowsBinding answers "would Resolve route this Model over this
// binding under this Policy?" — the per-binding decision inventory
// endpoints need, without picking a key. It mirrors the allowed-paths
// logic in Resolve (legacy + DSL + wildcard match against specific
// (provider, model, host) triples, plus the key-coverage gate) so the two
// stay in sync. PolicyAllows reduces it to "is there *any* enabled binding
// that passes?"
package routing

import (
	"github.com/wyolet/relay/app/binding"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
)

// PolicyAllows reports whether m is reachable through pol over any of its
// enabled bindings. Used to enumerate accessible models for inventory
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
	for _, hb := range snap.BindingsForModel(m.Meta.ID) {
		if hb.IsEnabled() && PolicyAllowsBinding(snap, pol, m, hb) {
			return true
		}
	}
	return false
}

// PolicyAllowsBinding reports whether pol would let Resolve route m over
// hb: first the key-coverage gate (a NoAuth host needs no HostKey — Resolve
// injects the anonymous one), then the grant check. Callers filter enabled
// bindings themselves.
func PolicyAllowsBinding(snap *appcatalog.Snapshot, pol *policy.Policy, m *model.Model, hb *binding.Binding) bool {
	if snap == nil || pol == nil || m == nil || hb == nil {
		return false
	}
	h, ok := snap.Host(hb.Spec.HostID)
	if !ok {
		return false
	}
	if !h.Spec.NoAuth && len(hostKeysForHost(snap, pol, hb.Spec.HostID)) == 0 {
		return false
	}
	if len(pol.Spec.ModelIDs) == 0 && len(pol.Spec.Models) == 0 {
		// Implicit wildcard: a NoAuth host skips the key gate that is such a
		// policy's only real authz, so reaching one takes an explicit grant.
		return (!isDeprecated(m) || pol.Spec.IncludeDeprecated) && !h.Spec.NoAuth
	}
	return snap.PolicyAllowsCombo(pol.Meta.ID, m.Meta.ID, hb.Spec.HostID)
}
