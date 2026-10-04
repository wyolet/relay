// PolicyAllowsBinding answers "would Resolve route this Model over this
// binding under this Policy?" — the per-binding decision inventory
// endpoints need, without picking a key. It mirrors the allowed-paths
// logic in Resolve (legacy + DSL + wildcard match against specific
// (provider, model, host) triples, plus the key + tier gate) so the two
// stay in sync. PolicyAllows reduces it to "is there *any* enabled binding
// that passes?"
//
// They are Resolver methods so listings obey the same options resolution
// does; every one is nil-safe (a nil Resolver applies the defaults).

package routing

import (
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
)

// PolicyAllows reports whether m is reachable through pol over any of its
// enabled bindings. Used to enumerate accessible models for inventory
// endpoints. Single-shot; not optimised for tight loops. userID is the
// calling user, which decides whether personal rows exist for the caller.
func (r *Resolver) PolicyAllows(snap *appcatalog.Snapshot, pol *policy.Policy, m *model.Model, userID string) bool {
	if pol == nil || m == nil || !m.IsEnabled() || !pol.IsEnabled() {
		return false
	}
	for _, hb := range snap.BindingsForModel(m.Meta.ID) {
		if hb.IsEnabled() && r.PolicyAllowsBinding(snap, pol, m, hb, userID) {
			return true
		}
	}
	return false
}

// PolicyAllowsBinding reports whether pol would let Resolve route m over
// hb: first the grant check, then the same key + tier gate Resolve applies
// (a NoAuth host yields the anonymous key), so a listed binding is one the
// caller can actually reach. Callers filter enabled bindings themselves.
func (r *Resolver) PolicyAllowsBinding(snap *appcatalog.Snapshot, pol *policy.Policy, m *model.Model, hb *binding.Binding, userID string) bool {
	if snap == nil || pol == nil || m == nil || hb == nil || !pol.IsEnabled() {
		return false
	}
	h, ok := snap.Host(hb.Spec.HostID)
	if !ok || !r.personalRowsVisible(hb, h, userID) {
		return false
	}
	var allowed bool
	if len(pol.Spec.ModelIDs) == 0 && len(pol.Spec.Models) == 0 {
		// Implicit wildcard: a NoAuth host skips the key gate that is such a
		// policy's only real authz, so reaching one takes an explicit grant.
		allowed = (!isDeprecated(m) || pol.Spec.IncludeDeprecated) && !h.Spec.NoAuth
	} else {
		allowed = snap.PolicyAllowsCombo(pol.Meta.ID, m.Meta.ID, hb.Spec.HostID)
	}
	return allowed && len(candidateKeys(snap, pol, m, h)) > 0
}

// PolicylessAllows reports whether m is reachable by a request that resolved
// no policy — the inventory question matching resolvePolicyless, which is the
// only thing that serves such a request. adapter narrows the answer to
// bindings declaring that wire shape; empty accepts any. userID is the calling
// user, scoping the pool exactly as resolution does.
//
// Mirrors resolvePolicyless step for step: enabled model, not deprecated,
// enabled binding, resolvable host, and a key the policy-less pool yields.
func (r *Resolver) PolicylessAllows(snap *appcatalog.Snapshot, m *model.Model, adapter adapters.Name, userID string) bool {
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
		if r.PolicylessAllowsBinding(snap, m, hb, userID) {
			return true
		}
	}
	return false
}

// PolicylessAllowsBinding is the per-binding form of PolicylessAllows: would
// resolvePolicyless route m over hb for userID. Callers filter enabled
// bindings themselves.
func (r *Resolver) PolicylessAllowsBinding(snap *appcatalog.Snapshot, m *model.Model, hb *binding.Binding, userID string) bool {
	if snap == nil || m == nil || hb == nil || !m.IsEnabled() || isDeprecated(m) {
		return false
	}
	h, ok := snap.Host(hb.Spec.HostID)
	if !ok || !r.personalRowsVisible(hb, h, userID) {
		return false
	}
	return len(policylessKeys(snap, m, h, userID)) > 0
}
