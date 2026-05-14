package catalog

import (
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
)

// build assembles a Snapshot from already enabled-filtered rows. Every row
// passed in enters the snapshot — there is no reachability pruning.
//
// Reverse joins (modelsByPolicy, pricingByModelHost) are derived from cross-
// refs at the end. A ref to a row that's not in the snapshot is silently
// skipped — that's how cascade invalidation degrades after a parent disables.
// validateCross rejects dangling refs before we get here, so this is a
// defensive guard only.
func build(
	provs []*provider.Provider,
	hosts []*host.Host,
	pols []*policy.Policy,
	rks []*relaykey.RelayKey,
	models []*model.Model,
	keys []*hostkey.HostKey,
	rls []*ratelimit.RateLimit,
	pricings []*pricing.Pricing,
) *Snapshot {
	s := &Snapshot{
		providersByID:      make(map[string]*provider.Provider, len(provs)),
		providersByName:    make(map[string]*provider.Provider, len(provs)),
		hostsByID:          make(map[string]*host.Host, len(hosts)),
		hostsByName:        make(map[string]*host.Host, len(hosts)),
		policiesByID:       make(map[string]*policy.Policy, len(pols)),
		policiesByName:     make(map[string]*policy.Policy, len(pols)),
		modelsByID:         make(map[string]*model.Model, len(models)),
		modelsByName:       map[string][]*model.Model{},
		hostKeysByID:       make(map[string]*hostkey.HostKey, len(keys)),
		rateLimitsByID:     make(map[string]*ratelimit.RateLimit, len(rls)),
		rateLimitsByName:   make(map[string]*ratelimit.RateLimit, len(rls)),
		relayKeysByID:      make(map[string]*relaykey.RelayKey, len(rks)),
		relayKeysByHash:    make(map[string]*relaykey.RelayKey, len(rks)),
		modelsByPolicy:     map[string][]*model.Model{},
		hostKeysByPolicy:   map[string][]*hostkey.HostKey{},
		rateLimitByPolicy:  map[string]*ratelimit.RateLimit{},
		pricingsByID:       make(map[string]*pricing.Pricing, len(pricings)),
		pricingByModelHost: map[string]*pricing.Pricing{},
		refsByProvider:     map[string]refSet{},
		refsByHost:         map[string]refSet{},
		refsByModel:        map[string]refSet{},
		refsByHostKey:      map[string]refSet{},
		refsByRateLimit:    map[string]refSet{},
		refsByPolicy:       map[string]refSet{},
	}

	for _, p := range provs {
		s.providersByID[p.Meta.ID] = p
		s.providersByName[p.Meta.Name] = p
	}
	for _, h := range hosts {
		s.hostsByID[h.Meta.ID] = h
		s.hostsByName[h.Meta.Name] = h
	}
	for _, p := range pols {
		s.policiesByID[p.Meta.ID] = p
		s.policiesByName[p.Meta.Name] = p
		s.registerRefs(refKey{Kind: refPolicy, ID: p.Meta.ID}, outboundPolicyRefs(p))
	}
	for _, k := range rks {
		s.relayKeysByID[k.Meta.ID] = k
		if k.Spec.KeyHash != "" {
			s.relayKeysByHash[k.Spec.KeyHash] = k
		}
		s.registerRefs(refKey{Kind: refRelayKey, ID: k.Meta.ID}, outboundRelayKeyRefs(k))
	}
	for _, m := range models {
		s.modelsByID[m.Meta.ID] = m
		// Index by the slug so callers can address the model with
		// `"model": "<slug>"`. Aliases extend addressability — a model
		// can be reached by both its slug and any declared alias.
		s.modelsByName[m.Meta.Name] = append(s.modelsByName[m.Meta.Name], m)
		for _, a := range m.Spec.Aliases {
			if a == m.Meta.Name {
				continue // already indexed via slug
			}
			s.modelsByName[a] = append(s.modelsByName[a], m)
		}
		s.registerRefs(refKey{Kind: refModel, ID: m.Meta.ID}, outboundModelRefs(m))
	}
	for _, k := range keys {
		s.hostKeysByID[k.Meta.ID] = k
		s.registerRefs(refKey{Kind: refHostKey, ID: k.Meta.ID}, outboundHostKeyRefs(k))
	}
	for _, r := range rls {
		s.rateLimitsByID[r.Meta.ID] = r
		s.rateLimitsByName[r.Meta.Name] = r
	}

	// Reverse joins per Policy. Skip refs to rows that aren't in the
	// snapshot (defensive; validateCross already rejected dangling refs).
	for _, p := range pols {
		for _, id := range p.Spec.ModelIDs {
			if m, ok := s.modelsByID[id]; ok {
				s.modelsByPolicy[p.Meta.ID] = append(s.modelsByPolicy[p.Meta.ID], m)
			}
		}
		for _, id := range p.Spec.HostKeyIDs {
			if k, ok := s.hostKeysByID[id]; ok {
				s.hostKeysByPolicy[p.Meta.ID] = append(s.hostKeysByPolicy[p.Meta.ID], k)
			}
		}
		if p.Spec.RateLimitID != "" {
			if r, ok := s.rateLimitsByID[p.Spec.RateLimitID]; ok {
				s.rateLimitByPolicy[p.Meta.ID] = r
			}
		}
	}

	for _, p := range pricings {
		s.pricingsByID[p.Meta.ID] = p
		hostID := p.Meta.Owner.ID
		for _, modelID := range p.Spec.TargetModelIDs {
			if _, ok := s.modelsByID[modelID]; !ok {
				continue
			}
			s.pricingByModelHost[modelID+"|"+hostID] = p
		}
		s.registerRefs(refKey{Kind: refPricing, ID: p.Meta.ID}, outboundPricingRefs(p))
	}

	return s
}
