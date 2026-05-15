package catalog

import (
	"fmt"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/relaykey"
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
	if _, ok := s.hostsByID[k.Spec.HostID]; !ok {
		return fmt.Errorf("hostkey %q: spec.hostId %q does not resolve", k.Meta.Name, k.Spec.HostID)
	}
	if _, ok := s.policiesByID[k.Spec.PolicyID]; !ok {
		return fmt.Errorf("hostkey %q: spec.policyId %q does not resolve", k.Meta.Name, k.Spec.PolicyID)
	}
	return nil
}

func validatePolicyInSnap(_ *policy.Policy, _ *Snapshot) error { return nil }

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

func validateRelayKeyInSnap(k *relaykey.RelayKey, s *Snapshot) error {
	if _, ok := s.policiesByID[k.Spec.PolicyID]; !ok {
		return fmt.Errorf("relaykey %q: policyId %q does not resolve", k.Meta.Name, k.Spec.PolicyID)
	}
	return nil
}
