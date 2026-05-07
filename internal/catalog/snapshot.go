package catalog

import "sort"

// snapshot is the in-memory view of the catalog, shared by YAMLStore and PGStore.
type snapshot struct {
	providers       map[string]*Provider
	models          map[string]*Model
	routes          map[string]*Route
	rateLimits      map[string]*RateLimit
	secrets         map[string]*Secret
	pools           map[string]*Pool
	effectivePrices map[string]*Pricing // keyed by model name; populated by buildEffectivePricing
}

func labelsMatch(selector, labels map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func newSnapshot() *snapshot {
	return &snapshot{
		providers:       map[string]*Provider{},
		models:          map[string]*Model{},
		routes:          map[string]*Route{},
		rateLimits:      map[string]*RateLimit{},
		secrets:         map[string]*Secret{},
		pools:           map[string]*Pool{},
		effectivePrices: map[string]*Pricing{},
	}
}

// buildEffectivePricing pre-computes the merged pricing for every model.
// Must be called after providers and models are fully populated.
func (s *snapshot) buildEffectivePricing() {
	s.effectivePrices = make(map[string]*Pricing, len(s.models))
	for name, m := range s.models {
		p := s.providers[m.Spec.Provider]
		ep := effectivePricing(p, m)
		if ep != nil {
			s.effectivePrices[name] = ep
		}
	}
}

// effectivePricing merges Provider.DefaultPricing with Model.Spec.Pricing.
// Model-level values win on collision. Returns nil if neither is set.
func effectivePricing(p *Provider, m *Model) *Pricing {
	var provPricing *Pricing
	if p != nil {
		provPricing = p.Spec.DefaultPricing
	}
	modelPricing := m.Spec.Pricing

	if provPricing == nil && modelPricing == nil {
		return nil
	}
	if provPricing == nil {
		return modelPricing
	}
	if modelPricing == nil {
		return provPricing
	}

	// Merge: start with a copy of provider pricing, overlay model.
	merged := &Pricing{
		Currency: provPricing.Currency,
		Unit:     provPricing.Unit,
		Rates:    make(map[string]float64, len(provPricing.Rates)+len(modelPricing.Rates)),
	}
	for k, v := range provPricing.Rates {
		merged.Rates[k] = v
	}
	// Model wins.
	if modelPricing.Currency != "" {
		merged.Currency = modelPricing.Currency
	}
	if modelPricing.Unit != "" {
		merged.Unit = modelPricing.Unit
	}
	for k, v := range modelPricing.Rates {
		merged.Rates[k] = v
	}
	return merged
}

// effectivePricingByModel returns the pre-computed effective pricing for a model.
func (s *snapshot) effectivePricingByModel(modelName string) (*Pricing, bool) {
	ep, ok := s.effectivePrices[modelName]
	return ep, ok
}

func (s *snapshot) providerByName(name string) (*Provider, bool) {
	p, ok := s.providers[name]
	return p, ok
}

func (s *snapshot) modelByName(name string) (*Model, bool) {
	if m, ok := s.models[name]; ok {
		return m, true
	}
	for _, m := range s.models {
		for _, a := range m.Spec.Aliases {
			if a == name {
				return m, true
			}
		}
	}
	return nil, false
}

func (s *snapshot) routeByName(name string) (*Route, bool) {
	r, ok := s.routes[name]
	return r, ok
}

func (s *snapshot) rateLimitByName(name string) (*RateLimit, bool) {
	rl, ok := s.rateLimits[name]
	return rl, ok
}

func (s *snapshot) secretByName(name string) (*Secret, bool) {
	sec, ok := s.secrets[name]
	return sec, ok
}

func (s *snapshot) poolByName(name string) (*Pool, bool) {
	p, ok := s.pools[name]
	return p, ok
}

func (s *snapshot) listProviders() []*Provider {
	out := make([]*Provider, 0, len(s.providers))
	for _, p := range s.providers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
}

func (s *snapshot) listModels() []*Model {
	out := make([]*Model, 0, len(s.models))
	for _, m := range s.models {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
}

func (s *snapshot) listRoutes() []*Route {
	out := make([]*Route, 0, len(s.routes))
	for _, r := range s.routes {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
}

func (s *snapshot) listRateLimits() []*RateLimit {
	out := make([]*RateLimit, 0, len(s.rateLimits))
	for _, rl := range s.rateLimits {
		out = append(out, rl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
}

func (s *snapshot) listSecrets() []*Secret {
	out := make([]*Secret, 0, len(s.secrets))
	for _, sec := range s.secrets {
		out = append(out, sec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
}

func (s *snapshot) listPools() []*Pool {
	out := make([]*Pool, 0, len(s.pools))
	for _, p := range s.pools {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
}

func (s *snapshot) secretsForPool(p *Pool) []*Secret {
	seen := map[string]struct{}{}
	var out []*Secret
	for _, name := range p.Spec.Secrets {
		sec, ok := s.secrets[name]
		if !ok {
			continue
		}
		if _, dup := seen[name]; !dup {
			seen[name] = struct{}{}
			out = append(out, sec)
		}
	}
	if len(p.Spec.SecretSelector) > 0 {
		for _, sec := range s.secrets {
			if _, dup := seen[sec.Metadata.Name]; dup {
				continue
			}
			if labelsMatch(p.Spec.SecretSelector, sec.Metadata.Labels) {
				seen[sec.Metadata.Name] = struct{}{}
				out = append(out, sec)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
}

func (s *snapshot) rateLimitsForRequest(provider *Provider, pool *Pool, model *Model, secret *Secret) []ResolvedRule {
	var out []ResolvedRule

	expand := func(parentKind Kind, parentName string, attachments []RateLimitAttachment) {
		for _, a := range attachments {
			rl, ok := s.rateLimits[a.Ref]
			if !ok {
				continue
			}
			for _, rule := range rl.Spec.NormalizedRules() {
				out = append(out, ResolvedRule{
					ParentKind:    parentKind,
					ParentName:    parentName,
					RateLimitName: rl.Metadata.Name,
					Strategy:      rl.Spec.Strategy,
					Window:        rl.Spec.Window,
					Rule:          rule,
					RateLimit:     rl,
					Meter:         Meter(rule.Meter),
				})
			}
		}
	}

	if secret != nil {
		expand(KindSecret, secret.Metadata.Name, secret.Spec.RateLimits)
	}
	if pool != nil {
		expand(KindPool, pool.Metadata.Name, pool.Spec.RateLimits)
	}
	if model != nil {
		expand(KindModel, model.Metadata.Name, model.Spec.RateLimits)
	}
	return out
}

func (s *snapshot) defaultProvider() *Provider {
	for _, p := range s.providers {
		if p.Spec.Default {
			return p
		}
	}
	return nil
}

func (s *snapshot) defaultRoute() *Route {
	for _, r := range s.routes {
		if r.Spec.Default {
			return r
		}
	}
	return nil
}

func (s *snapshot) providerForModel(modelName string) (*Provider, bool) {
	m, ok := s.models[modelName]
	if !ok {
		return nil, false
	}
	p, ok := s.providers[m.Spec.Provider]
	return p, ok
}
