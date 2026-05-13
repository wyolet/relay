package catalog

import (
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/providerkey"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
)

// build assembles a Snapshot from the (already enabled-filtered) rows.
// Reachability pruning happens here: Models / ProviderKeys / RateLimits not
// referenced by any input Policy are dropped.
//
// The caller is responsible for filtering rows by Spec.Enabled before
// passing them in. build does not consult Enabled itself — it trusts inputs.
func build(
	pols []*policy.Policy,
	rks []*relaykey.RelayKey,
	models []*model.Model,
	keys []*providerkey.ProviderKey,
	rls []*ratelimit.RateLimit,
) *Snapshot {
	s := &Snapshot{
		policiesByID:         make(map[string]*policy.Policy, len(pols)),
		policiesByName:       make(map[string]*policy.Policy, len(pols)),
		modelsByID:           map[string]*model.Model{},
		modelsByName:         map[string]*model.Model{},
		providerKeysByID:     map[string]*providerkey.ProviderKey{},
		rateLimitsByID:       map[string]*ratelimit.RateLimit{},
		relayKeysByID:        make(map[string]*relaykey.RelayKey, len(rks)),
		relayKeysByHash:      make(map[string]*relaykey.RelayKey, len(rks)),
		modelsByPolicy:       map[string][]*model.Model{},
		providerKeysByPolicy: map[string][]*providerkey.ProviderKey{},
		rateLimitByPolicy:    map[string]*ratelimit.RateLimit{},
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
		for _, id := range p.Spec.ProviderKeyIDs {
			wantKey[id] = struct{}{}
		}
		if p.Spec.RateLimitID != "" {
			wantRL[p.Spec.RateLimitID] = struct{}{}
		}
	}

	// Index reachable Models by id, slug, and aliases.
	for _, m := range models {
		if _, ok := wantModel[m.Meta.ID]; !ok {
			continue
		}
		s.modelsByID[m.Meta.ID] = m
		s.modelsByName[m.Meta.Name] = m
		for _, a := range m.Spec.Aliases {
			s.modelsByName[a] = m
		}
	}

	// Index reachable ProviderKeys.
	for _, k := range keys {
		if _, ok := wantKey[k.Meta.ID]; ok {
			s.providerKeysByID[k.Meta.ID] = k
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
		for _, id := range p.Spec.ProviderKeyIDs {
			if k, ok := s.providerKeysByID[id]; ok {
				s.providerKeysByPolicy[p.Meta.ID] = append(s.providerKeysByPolicy[p.Meta.ID], k)
			}
		}
		if p.Spec.RateLimitID != "" {
			if r, ok := s.rateLimitsByID[p.Spec.RateLimitID]; ok {
				s.rateLimitByPolicy[p.Meta.ID] = r
			}
		}
	}

	return s
}
