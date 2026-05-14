package catalog

import (
	"fmt"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/relaykey"
)

// validateHostInSnap checks cross-refs for a single Host against snap.
// Enforces the host-policies menu invariant:
//   - Every id in Spec.Policies resolves to an enabled host-owned Policy
//     whose Owner.ID is this host.
//   - Spec.DefaultPolicy, when set, is one of Spec.Policies.
func validateHostInSnap(h *host.Host, s *Snapshot) error {
	menu := make(map[string]struct{}, len(h.Spec.Policies))
	for _, polID := range h.Spec.Policies {
		pol, ok := s.policiesByID[polID]
		if !ok {
			return fmt.Errorf("host %q: policies references unknown or disabled policy %q", h.Meta.Name, polID)
		}
		if pol.Meta.Owner.Kind != meta.OwnerHost || pol.Meta.Owner.ID != h.Meta.ID {
			return fmt.Errorf("host %q: policy %q is not host-owned by this host (owner=%s/%s)",
				h.Meta.Name, pol.Meta.Name, pol.Meta.Owner.Kind, pol.Meta.Owner.ID)
		}
		menu[polID] = struct{}{}
	}
	if h.Spec.DefaultPolicy != "" {
		if _, ok := menu[h.Spec.DefaultPolicy]; !ok {
			return fmt.Errorf("host %q: defaultPolicy %q is not in spec.policies", h.Meta.Name, h.Spec.DefaultPolicy)
		}
	}
	return nil
}

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
	if _, ok := s.hostsByID[k.Spec.HostID]; !ok {
		return fmt.Errorf("hostkey %q: spec.hostId %q does not match any enabled Host", k.Meta.Name, k.Spec.HostID)
	}
	pol, ok := s.policiesByID[k.Spec.PolicyID]
	if !ok {
		return fmt.Errorf("hostkey %q: spec.policyId %q references unknown or disabled policy", k.Meta.Name, k.Spec.PolicyID)
	}
	if pol.Meta.Owner.Kind != meta.OwnerHost || pol.Meta.Owner.ID != k.Spec.HostID {
		return fmt.Errorf("hostkey %q: policy %q is not host-owned by host %q (owner=%s/%s)",
			k.Meta.Name, pol.Meta.Name, k.Spec.HostID, pol.Meta.Owner.Kind, pol.Meta.Owner.ID)
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
