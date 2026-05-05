package configstore

import "sort"

// snapshot is the in-memory view of the catalog, shared by YAMLStore and PGStore.
type snapshot struct {
	providers  map[string]*Provider
	models     map[string]*Model
	routes     map[string]*Route
	rateLimits map[string]*RateLimit
	secrets    map[string]*Secret
	pools      map[string]*Pool
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
		providers:  map[string]*Provider{},
		models:     map[string]*Model{},
		routes:     map[string]*Route{},
		rateLimits: map[string]*RateLimit{},
		secrets:    map[string]*Secret{},
		pools:      map[string]*Pool{},
	}
}

func (s *snapshot) providerByName(name string) (*Provider, bool) {
	p, ok := s.providers[name]
	return p, ok
}

func (s *snapshot) modelByName(name string) (*Model, bool) {
	m, ok := s.models[name]
	return m, ok
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
	if secret != nil {
		for _, a := range secret.Spec.RateLimits {
			rl, ok := s.rateLimits[a.Ref]
			if !ok {
				continue
			}
			out = append(out, ResolvedRule{ParentKind: KindSecret, ParentName: secret.Metadata.Name, Meter: a.Meter, RateLimit: rl})
		}
	}
	if pool != nil {
		for _, a := range pool.Spec.RateLimits {
			rl, ok := s.rateLimits[a.Ref]
			if !ok {
				continue
			}
			out = append(out, ResolvedRule{ParentKind: KindPool, ParentName: pool.Metadata.Name, Meter: a.Meter, RateLimit: rl})
		}
	}
	if model != nil {
		for _, a := range model.Spec.RateLimits {
			rl, ok := s.rateLimits[a.Ref]
			if !ok {
				continue
			}
			out = append(out, ResolvedRule{ParentKind: KindModel, ParentName: model.Metadata.Name, Meter: a.Meter, RateLimit: rl})
		}
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
