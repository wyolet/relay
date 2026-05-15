package catalog

import "github.com/wyolet/relay/app/pricing"

func (s *Snapshot) addPricings(pricings []*pricing.Pricing, hosts, models idSet) {
	for _, p := range pricings {
		clean, keep := sanitizePricing(p, hosts, models)
		if !keep {
			continue
		}
		s.pricingsByID[clean.Meta.ID] = clean
		hostID := clean.Meta.Owner.ID
		for _, modelID := range clean.Spec.TargetModelIDs {
			if _, ok := s.modelsByID[modelID]; !ok {
				continue
			}
			s.pricingByModelHost[modelID+"|"+hostID] = clean
		}
		s.registerRefs(refKey{Kind: refPricing, ID: clean.Meta.ID}, outboundPricingRefs(p))
	}
}

// sanitizePricing drops the row if its owning Host is missing, or if every
// targeted Model is missing — without a model the rate sheet has no anchor
// to apply to.
func sanitizePricing(p *pricing.Pricing, hosts, models idSet) (*pricing.Pricing, bool) {
	if _, ok := hosts[p.Meta.Owner.ID]; !ok {
		return nil, false
	}
	clean := *p
	clean.Spec = p.Spec
	clean.Spec.TargetModelIDs = filterIDs(p.Spec.TargetModelIDs, models)
	if len(clean.Spec.TargetModelIDs) == 0 {
		return nil, false
	}
	return &clean, true
}
