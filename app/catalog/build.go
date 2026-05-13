package catalog

import (
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
)

// build assembles a Snapshot from the (already enabled-filtered) rows.
// Reachability pruning happens here: Models / HostKeys / RateLimits not
// referenced by any input Policy are dropped.
//
// Models are indexed by every entry in their Spec.Aliases verbatim — no
// prefix derivation. The operator decides what wire strings address each
// Model. Multiple Models may share an alias intentionally (same wire name
// hosted by different Providers); ModelsByName returns the full list and
// the consumer disambiguates with a suffix or header.
//
// providerSlugByID is carried through to the Snapshot for hot-path
// disambiguation; the Provider rows themselves never enter the Snapshot.
//
// The caller is responsible for filtering rows by Spec.Enabled before
// passing them in. build does not consult Enabled itself — it trusts inputs.
func build(
	pols []*policy.Policy,
	rks []*relaykey.RelayKey,
	models []*model.Model,
	keys []*hostkey.HostKey,
	rls []*ratelimit.RateLimit,
	pricings []*pricing.Pricing,
	providerSlugByID map[string]string,
	hostSlugByID map[string]string,
) *Snapshot {
	s := &Snapshot{
		policiesByID:       make(map[string]*policy.Policy, len(pols)),
		policiesByName:     make(map[string]*policy.Policy, len(pols)),
		modelsByID:         map[string]*model.Model{},
		modelsByName:       map[string][]*model.Model{},
		hostKeysByID:       map[string]*hostkey.HostKey{},
		rateLimitsByID:     map[string]*ratelimit.RateLimit{},
		relayKeysByID:      make(map[string]*relaykey.RelayKey, len(rks)),
		relayKeysByHash:    make(map[string]*relaykey.RelayKey, len(rks)),
		providerSlugByID:   providerSlugByID,
		hostSlugByID:       hostSlugByID,
		modelsByPolicy:     map[string][]*model.Model{},
		hostKeysByPolicy:   map[string][]*hostkey.HostKey{},
		rateLimitByPolicy:  map[string]*ratelimit.RateLimit{},
		pricingsByID:       make(map[string]*pricing.Pricing, len(pricings)),
		pricingByModelHost: map[string]*pricing.Pricing{},
	}

	// Policies always enter wholesale (input is already enabled-filtered).
	for _, p := range pols {
		s.policiesByID[p.Meta.ID] = p
		s.policiesByName[p.Meta.Name] = p
	}

	// RelayKeys always enter wholesale.
	for _, k := range rks {
		s.relayKeysByID[k.Meta.ID] = k
		if k.Spec.KeyHash != "" {
			s.relayKeysByHash[k.Spec.KeyHash] = k
		}
	}

	// Reachability sets: union over all input Policies' refs.
	wantModel := map[string]struct{}{}
	wantKey := map[string]struct{}{}
	wantRL := map[string]struct{}{}
	for _, p := range pols {
		for _, id := range p.Spec.ModelIDs {
			wantModel[id] = struct{}{}
		}
		for _, id := range p.Spec.HostKeyIDs {
			wantKey[id] = struct{}{}
		}
		if p.Spec.RateLimitID != "" {
			wantRL[p.Spec.RateLimitID] = struct{}{}
		}
	}

	// Index reachable Models by id and by every entry in Spec.Aliases
	// verbatim. Aliases may collide intentionally (same wire name hosted
	// by multiple Providers); the index is multivalued and the consumer
	// disambiguates downstream.
	for _, m := range models {
		if _, ok := wantModel[m.Meta.ID]; !ok {
			continue
		}
		s.modelsByID[m.Meta.ID] = m
		for _, a := range m.Spec.Aliases {
			s.modelsByName[a] = append(s.modelsByName[a], m)
		}
	}

	// Index reachable HostKeys.
	for _, k := range keys {
		if _, ok := wantKey[k.Meta.ID]; ok {
			s.hostKeysByID[k.Meta.ID] = k
		}
	}

	// Index reachable RateLimits.
	for _, r := range rls {
		if _, ok := wantRL[r.Meta.ID]; ok {
			s.rateLimitsByID[r.Meta.ID] = r
		}
	}

	// Reverse joins per Policy. Skip refs that didn't survive reachability
	// (shouldn't happen post-validate, but defensive).
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

	// Index pricings by id and by (targetModelID, hostID). Drop entries whose
	// target model didn't survive reachability — defensive guard only;
	// validateCross already rejected unknown models.
	for _, p := range pricings {
		s.pricingsByID[p.Meta.ID] = p
		hostID := p.Meta.Owner.ID
		for _, modelID := range p.Spec.TargetModelIDs {
			if _, ok := s.modelsByID[modelID]; !ok {
				continue
			}
			s.pricingByModelHost[modelID+"|"+hostID] = p
		}
	}

	return s
}
