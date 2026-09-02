package catalog

import (
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
)

// refKind labels a referenced row's kind. Keep this in sync with the
// snapshot's index of refsBy* maps — each parent kind has one map keyed
// by parent id, valued by the set of (childKind, childID) that depend on it.
type refKind string

const (
	refProvider  refKind = "provider"
	refHost      refKind = "host"
	refModel     refKind = "model"
	refHostKey   refKind = "hostkey"
	refRateLimit refKind = "ratelimit"
	refPolicy    refKind = "policy"
	refPricing   refKind = "pricing"
	refRelayKey  refKind = "relaykey"
	refBinding   refKind = "binding"
	refTeam      refKind = "team"
	refProject   refKind = "project"
	// refServiceAccount keeps the "serviceaccount" value while the
	// identifier for a Key stays refRelayKey — refKey is the struct.
	refServiceAccount refKind = "serviceaccount"
	refRole           refKind = "role"
	refRoleBinding    refKind = "rolebinding"
	refPolicyBinding  refKind = "policybinding"
)

// refKey identifies one row by (kind, id). Used as the value type in
// reverse-ref sets.
type refKey struct {
	Kind refKind
	ID   string
}

// refSet is the value type stored in the snapshot's refsBy* maps. A nil
// refSet is fine to read from but cannot be written to.
type refSet map[refKey]struct{}

func (r refSet) add(k refKey)    { r[k] = struct{}{} }
func (r refSet) remove(k refKey) { delete(r, k) }

// outboundRefs returns every row referenced by the given row, by kind.
// Each function below knows the row's spec and returns the (parent kind,
// parent id) tuples it depends on.

func outboundModelRefs(m *model.Model) []refKey {
	if m.Meta.Owner.Kind == meta.OwnerProvider && m.Meta.Owner.ID != "" {
		return []refKey{{Kind: refProvider, ID: m.Meta.Owner.ID}}
	}
	return nil
}

func outboundHostKeyRefs(k *hostkey.HostKey) []refKey {
	refs := make([]refKey, 0, 3)
	if k.Meta.Owner.Kind == meta.OwnerProject && k.Meta.Owner.ID != "" {
		refs = append(refs, refKey{Kind: refProject, ID: k.Meta.Owner.ID})
	}
	if k.Spec.HostID != "" {
		refs = append(refs, refKey{Kind: refHost, ID: k.Spec.HostID})
	}
	if k.Spec.PolicyID != "" {
		refs = append(refs, refKey{Kind: refPolicy, ID: k.Spec.PolicyID})
	}
	if len(refs) == 0 {
		return nil
	}
	return refs
}

func outboundPolicyRefs(p *policy.Policy) []refKey {
	refs := make([]refKey, 0, len(p.Spec.ModelIDs)+len(p.Spec.HostKeyIDs)+2)
	if p.Meta.Owner.Kind == meta.OwnerProject && p.Meta.Owner.ID != "" {
		refs = append(refs, refKey{Kind: refProject, ID: p.Meta.Owner.ID})
	}
	for _, id := range p.Spec.ModelIDs {
		refs = append(refs, refKey{Kind: refModel, ID: id})
	}
	for _, id := range p.Spec.HostKeyIDs {
		refs = append(refs, refKey{Kind: refHostKey, ID: id})
	}
	if p.Spec.RateLimitID != "" {
		refs = append(refs, refKey{Kind: refRateLimit, ID: p.Spec.RateLimitID})
	}
	return refs
}

func outboundPricingRefs(p *pricing.Pricing) []refKey {
	refs := make([]refKey, 0, 1+len(p.Spec.TargetModelIDs))
	if p.Meta.Owner.ID != "" {
		refs = append(refs, refKey{Kind: refHost, ID: p.Meta.Owner.ID})
	}
	for _, id := range p.Spec.TargetModelIDs {
		refs = append(refs, refKey{Kind: refModel, ID: id})
	}
	return refs
}

// outboundBindingRefs returns the parents a binding depends on: its Model
// and Host (both ref-parent kinds). PricingID is not a ref-parent kind —
// pricing is itself a child of host+model — so a dropped pricing is handled
// by sanitizeBinding clearing the ref, not by reverse-ref eviction.
func outboundBindingRefs(b *binding.Binding) []refKey {
	refs := make([]refKey, 0, 2)
	if b.Spec.ModelID != "" {
		refs = append(refs, refKey{Kind: refModel, ID: b.Spec.ModelID})
	}
	if b.Spec.HostID != "" {
		refs = append(refs, refKey{Kind: refHost, ID: b.Spec.HostID})
	}
	return refs
}

// outboundRateLimitRefs returns the owning Project of a project-owned rate
// limit. Rate limits owned by any other kind have no parents.
func outboundRateLimitRefs(r *ratelimit.RateLimit) []refKey {
	if r.Meta.Owner.Kind == meta.OwnerProject && r.Meta.Owner.ID != "" {
		return []refKey{{Kind: refProject, ID: r.Meta.Owner.ID}}
	}
	return nil
}

// outboundProjectRefs returns the Team a project belongs to. A project
// without its team is dropped, so the edge is hard.
func outboundProjectRefs(p *project.Project) []refKey {
	return []refKey{{Kind: refTeam, ID: p.Spec.TeamID}}
}

// outboundKeyRefs returns the Policy override (when set) and the
// ServiceAccount principal. A project-owned key reaches its project
// through the account, so there is no separate project edge.
func outboundKeyRefs(k *key.Key) []refKey {
	refs := make([]refKey, 0, 2)
	if k.Spec.PolicyID != "" {
		refs = append(refs, refKey{Kind: refPolicy, ID: k.Spec.PolicyID})
	}
	if k.Spec.Principal.Kind == key.PrincipalServiceAccount && k.Spec.Principal.ID != "" {
		refs = append(refs, refKey{Kind: refServiceAccount, ID: k.Spec.Principal.ID})
	}
	if len(refs) == 0 {
		return nil
	}
	return refs
}

// outboundServiceAccountRefs returns the owning Project and the optional
// policy override. Both are hard: a dangling either way drops the row.
func outboundServiceAccountRefs(sa *serviceaccount.ServiceAccount) []refKey {
	refs := []refKey{{Kind: refProject, ID: sa.Spec.ProjectID}}
	if sa.Spec.PolicyID != "" {
		refs = append(refs, refKey{Kind: refPolicy, ID: sa.Spec.PolicyID})
	}
	return refs
}

// outboundRoleBindingRefs returns the granted Role and, for a scoped
// binding, its scope target. Both are hard: without either there is
// nothing to grant.
func outboundRoleBindingRefs(b *rolebinding.RoleBinding) []refKey {
	refs := []refKey{{Kind: refRole, ID: b.Spec.RoleID}}
	switch b.Spec.Scope.Kind {
	case meta.OwnerTeam:
		refs = append(refs, refKey{Kind: refTeam, ID: b.Spec.Scope.ID})
	case meta.OwnerProject:
		refs = append(refs, refKey{Kind: refProject, ID: b.Spec.Scope.ID})
	}
	return refs
}

// outboundPolicyBindingRefs returns the Project the binding lives in and
// the Policy it points at. Both hard.
func outboundPolicyBindingRefs(b *policybinding.PolicyBinding) []refKey {
	return []refKey{
		{Kind: refProject, ID: b.Spec.ProjectID},
		{Kind: refPolicy, ID: b.Spec.PolicyID},
	}
}

// registerRefs records that `child` depends on every entry in `parents`.
// The snapshot's refsBy* maps are mutated; safe to call only when the
// caller owns this Snapshot (e.g. during build or COW reconcile).
func (s *Snapshot) registerRefs(child refKey, parents []refKey) {
	for _, p := range parents {
		set := s.refsetFor(p.Kind, p.ID)
		if set == nil {
			continue
		}
		set.add(child)
	}
}

// unregisterRefs drops every (parent, child) edge. Mirrors registerRefs.
func (s *Snapshot) unregisterRefs(child refKey, parents []refKey) {
	for _, p := range parents {
		set := s.refsetFor(p.Kind, p.ID)
		if set == nil {
			continue
		}
		set.remove(child)
		if len(set) == 0 {
			s.dropRefset(p.Kind, p.ID)
		}
	}
}

// refsetFor returns the refSet bucket for (kind, id), creating it if absent.
// Returns nil only for unsupported kinds.
func (s *Snapshot) refsetFor(kind refKind, id string) refSet {
	var m map[string]refSet
	switch kind {
	case refProvider:
		m = s.refsByProvider
	case refHost:
		m = s.refsByHost
	case refModel:
		m = s.refsByModel
	case refHostKey:
		m = s.refsByHostKey
	case refRateLimit:
		m = s.refsByRateLimit
	case refPolicy:
		m = s.refsByPolicy
	case refTeam:
		m = s.refsByTeam
	case refProject:
		m = s.refsByProject
	case refServiceAccount:
		m = s.refsByServiceAccount
	case refRole:
		m = s.refsByRole
	default:
		return nil
	}
	if m == nil {
		return nil
	}
	set, ok := m[id]
	if !ok {
		set = refSet{}
		m[id] = set
	}
	return set
}

func (s *Snapshot) dropRefset(kind refKind, id string) {
	switch kind {
	case refProvider:
		delete(s.refsByProvider, id)
	case refHost:
		delete(s.refsByHost, id)
	case refModel:
		delete(s.refsByModel, id)
	case refHostKey:
		delete(s.refsByHostKey, id)
	case refRateLimit:
		delete(s.refsByRateLimit, id)
	case refPolicy:
		delete(s.refsByPolicy, id)
	case refTeam:
		delete(s.refsByTeam, id)
	case refProject:
		delete(s.refsByProject, id)
	case refServiceAccount:
		delete(s.refsByServiceAccount, id)
	case refRole:
		delete(s.refsByRole, id)
	}
}

// Dependents returns every refKey that depends on (kind, id). nil if none.
// The slice is freshly allocated; mutating it doesn't affect the snapshot.
func (s *Snapshot) Dependents(kind refKind, id string) []refKey {
	var m map[string]refSet
	switch kind {
	case refProvider:
		m = s.refsByProvider
	case refHost:
		m = s.refsByHost
	case refModel:
		m = s.refsByModel
	case refHostKey:
		m = s.refsByHostKey
	case refRateLimit:
		m = s.refsByRateLimit
	case refPolicy:
		m = s.refsByPolicy
	case refTeam:
		m = s.refsByTeam
	case refProject:
		m = s.refsByProject
	case refServiceAccount:
		m = s.refsByServiceAccount
	case refRole:
		m = s.refsByRole
	}
	set, ok := m[id]
	if !ok {
		return nil
	}
	out := make([]refKey, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}
