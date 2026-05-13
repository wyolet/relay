package catalog

import (
	"fmt"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/relaykey"
)

// validateModelInSnap checks cross-refs for a single Model against snap.
func validateModelInSnap(m *model.Model, s *Snapshot) error {
	if _, ok := s.providersByID[m.Meta.Owner.ID]; !ok {
		return fmt.Errorf("model %q: owner.id %q does not match any enabled Provider", m.Meta.Name, m.Meta.Owner.ID)
	}
	for _, b := range m.Spec.Hosts {
		if b.HostID == "" {
			continue
		}
		if _, ok := s.hostsByID[b.HostID]; !ok {
			return fmt.Errorf("model %q: host binding references unknown or disabled host %q", m.Meta.Name, b.HostID)
		}
	}
	if m.Spec.Deprecation != nil && m.Spec.Deprecation.Replacement != "" {
		if _, ok := s.modelsByID[m.Spec.Deprecation.Replacement]; !ok {
			return fmt.Errorf("model %q: deprecation.replacement references unknown or disabled model %q",
				m.Meta.Name, m.Spec.Deprecation.Replacement)
		}
	}
	return nil
}

// validateHostKeyInSnap checks cross-refs for a single HostKey against snap.
func validateHostKeyInSnap(k *hostkey.HostKey, s *Snapshot) error {
	if _, ok := s.hostsByID[k.Meta.Owner.ID]; !ok {
		return fmt.Errorf("hostkey %q: owner.id %q does not match any enabled Host", k.Meta.Name, k.Meta.Owner.ID)
	}
	return nil
}

// validatePolicyInSnap checks cross-refs for a single Policy against snap.
func validatePolicyInSnap(p *policy.Policy, s *Snapshot) error {
	for _, id := range p.Spec.ModelIDs {
		if _, ok := s.modelsByID[id]; !ok {
			return fmt.Errorf("policy %q: modelIds references unknown or disabled model %q", p.Meta.Name, id)
		}
	}
	for _, id := range p.Spec.HostKeyIDs {
		if _, ok := s.hostKeysByID[id]; !ok {
			return fmt.Errorf("policy %q: hostKeyIds references unknown or disabled key %q", p.Meta.Name, id)
		}
	}
	if p.Spec.RateLimitID != "" {
		if _, ok := s.rateLimitsByID[p.Spec.RateLimitID]; !ok {
			return fmt.Errorf("policy %q: rateLimitId references unknown or disabled rate limit %q", p.Meta.Name, p.Spec.RateLimitID)
		}
	}
	return nil
}

// validatePricingInSnap checks cross-refs for a single Pricing against snap.
// It also checks for duplicate (model, host) claims within snap.
func validatePricingInSnap(p *pricing.Pricing, s *Snapshot) error {
	if _, ok := s.hostsByID[p.Meta.Owner.ID]; !ok {
		return fmt.Errorf("pricing %q: owner.id %q does not match any enabled Host", p.Meta.Name, p.Meta.Owner.ID)
	}
	for _, modelID := range p.Spec.TargetModelIDs {
		if _, ok := s.modelsByID[modelID]; !ok {
			return fmt.Errorf("pricing %q: targetModel %q references unknown or disabled model", p.Meta.Name, modelID)
		}
		key := modelID + "|" + p.Meta.Owner.ID
		if existing, dup := s.pricingByModelHost[key]; dup && existing.Meta.ID != p.Meta.ID {
			return fmt.Errorf("duplicate pricing: pricing %q and %q both cover model %q for the same host",
				existing.Meta.Name, p.Meta.Name, modelID)
		}
	}
	return nil
}

// validateRelayKeyInSnap checks cross-refs for a single RelayKey against snap.
func validateRelayKeyInSnap(k *relaykey.RelayKey, s *Snapshot) error {
	if _, ok := s.policiesByID[k.Spec.PolicyID]; !ok {
		return fmt.Errorf("relaykey %q: policyId references unknown or disabled policy %q", k.Meta.Name, k.Spec.PolicyID)
	}
	return nil
}
