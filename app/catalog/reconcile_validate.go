package catalog

import (
	"fmt"

	"github.com/wyolet/relay/app/host"
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

// "Required-ref" validators used by the COW reconciler. A required ref is
// one whose absence makes the row unusable on the hot path — sanitize
// drops the row entirely rather than leaving a dangling reference. Soft
// refs (Policy.HostKeyIDs, Policy.RateLimitID, Host.Spec.Policies, etc.)
// are filtered by the per-kind sanitizers and never error here.
//
// Used both at upsert time (to decide whether the upsert lands or evicts)
// and during cascade invalidation (to decide whether a dependent row
// survives after a parent disappears).

func validateHostInSnap(_ *host.Host, _ *Snapshot) error { return nil }

func validateModelInSnap(m *model.Model, s *Snapshot) error {
	if _, ok := s.providersByID[m.Meta.Owner.ID]; !ok {
		return fmt.Errorf("model %q: owner.id %q does not match any enabled Provider", m.Meta.Name, m.Meta.Owner.ID)
	}
	return nil
}

func validateHostKeyInSnap(k *hostkey.HostKey, s *Snapshot) error {
	if err := validateOwnerProjectInSnap("hostkey", k.Meta, s); err != nil {
		return err
	}
	if _, ok := s.hostsByID[k.Spec.HostID]; !ok {
		return fmt.Errorf("hostkey %q: spec.hostId %q does not resolve", k.Meta.Name, k.Spec.HostID)
	}
	// Disabled included: the key survives its tier being switched off and
	// comes back when it returns. The tier gate denies it meanwhile, because
	// PolicyAllowsCombo grants nothing for a policy that is not enabled.
	pol, ok := s.policyLookup(k.Spec.PolicyID)
	if !ok {
		return fmt.Errorf("hostkey %q: spec.policyId %q does not resolve", k.Meta.Name, k.Spec.PolicyID)
	}
	// Same invariant sanitizeHostKey enforces, so a policy re-pointed at
	// another host evicts the keys that mirrored it.
	if pol.Meta.Owner.Kind != meta.OwnerHost || pol.Meta.Owner.ID != k.Spec.HostID {
		return fmt.Errorf("hostkey %q: policy %q is not host-owned by host %q", k.Meta.Name, pol.Meta.Name, k.Spec.HostID)
	}
	return nil
}

func validatePolicyInSnap(p *policy.Policy, s *Snapshot) error {
	return validateOwnerProjectInSnap("policy", p.Meta, s)
}

func validateRateLimitInSnap(r *ratelimit.RateLimit, s *Snapshot) error {
	return validateOwnerProjectInSnap("ratelimit", r.Meta, s)
}

func validateProjectInSnap(p *project.Project, s *Snapshot) error {
	if _, ok := s.teamsByID[p.Spec.TeamID]; !ok {
		return fmt.Errorf("project %q: spec.teamId %q does not resolve", p.Meta.Name, p.Spec.TeamID)
	}
	return nil
}

// validateOwnerProjectInSnap is the shared owner check for the kinds that
// may live inside a Project: the owning Project must still be present.
func validateOwnerProjectInSnap(kind string, m meta.Metadata, s *Snapshot) error {
	if m.Owner.Kind != meta.OwnerProject {
		return nil
	}
	if _, ok := s.projectsByID[m.Owner.ID]; !ok {
		return fmt.Errorf("%s %q: owner project %q does not resolve", kind, m.Name, m.Owner.ID)
	}
	return nil
}

func validatePricingInSnap(p *pricing.Pricing, s *Snapshot) error {
	if _, ok := s.hostsByID[p.Meta.Owner.ID]; !ok {
		return fmt.Errorf("pricing %q: owner.id %q does not resolve", p.Meta.Name, p.Meta.Owner.ID)
	}
	any := false
	for _, modelID := range p.Spec.TargetModelIDs {
		if _, ok := s.modelsByID[modelID]; ok {
			any = true
			break
		}
	}
	if !any {
		return fmt.Errorf("pricing %q: no resolvable targetModels", p.Meta.Name)
	}
	for _, modelID := range p.Spec.TargetModelIDs {
		if _, ok := s.modelsByID[modelID]; !ok {
			continue
		}
		key := modelID + "|" + p.Meta.Owner.ID
		if existing, dup := s.pricingByModelHost[key]; dup && existing.Meta.ID != p.Meta.ID {
			return fmt.Errorf("duplicate pricing: pricing %q and %q both cover model %q for the same host",
				existing.Meta.Name, p.Meta.Name, modelID)
		}
	}
	return nil
}

func validateKeyInSnap(k *key.Key, s *Snapshot) error {
	if k.Spec.PolicyID != "" && !s.policyResolvable(k.Spec.PolicyID) {
		return fmt.Errorf("key %q: policyId %q does not resolve", k.Meta.Name, k.Spec.PolicyID)
	}
	if k.Spec.Principal.Kind == key.PrincipalServiceAccount {
		if _, ok := s.serviceAccountsByID[k.Spec.Principal.ID]; !ok {
			return fmt.Errorf("key %q: principal serviceaccount %q does not resolve", k.Meta.Name, k.Spec.Principal.ID)
		}
	}
	return nil
}

func validateServiceAccountInSnap(sa *serviceaccount.ServiceAccount, s *Snapshot) error {
	if _, ok := s.projectsByID[sa.Spec.ProjectID]; !ok {
		return fmt.Errorf("serviceaccount %q: spec.projectId %q does not resolve", sa.Meta.Name, sa.Spec.ProjectID)
	}
	if sa.Spec.PolicyID != "" && !s.policyResolvable(sa.Spec.PolicyID) {
		return fmt.Errorf("serviceaccount %q: spec.policyId %q does not resolve", sa.Meta.Name, sa.Spec.PolicyID)
	}
	return nil
}

func validateRoleBindingInSnap(b *rolebinding.RoleBinding, s *Snapshot) error {
	if _, ok := s.rolesByID[b.Spec.RoleID]; !ok {
		return fmt.Errorf("rolebinding %q: spec.roleId %q does not resolve", b.Meta.Name, b.Spec.RoleID)
	}
	switch b.Spec.Scope.Kind {
	case meta.OwnerTeam:
		if _, ok := s.teamsByID[b.Spec.Scope.ID]; !ok {
			return fmt.Errorf("rolebinding %q: scope team %q does not resolve", b.Meta.Name, b.Spec.Scope.ID)
		}
	case meta.OwnerProject:
		if _, ok := s.projectsByID[b.Spec.Scope.ID]; !ok {
			return fmt.Errorf("rolebinding %q: scope project %q does not resolve", b.Meta.Name, b.Spec.Scope.ID)
		}
	}
	return nil
}

func validatePolicyBindingInSnap(b *policybinding.PolicyBinding, s *Snapshot) error {
	if _, ok := s.projectsByID[b.Spec.ProjectID]; !ok {
		return fmt.Errorf("policybinding %q: spec.projectId %q does not resolve", b.Meta.Name, b.Spec.ProjectID)
	}
	if !s.policyResolvable(b.Spec.PolicyID) {
		return fmt.Errorf("policybinding %q: spec.policyId %q does not resolve", b.Meta.Name, b.Spec.PolicyID)
	}
	return nil
}
