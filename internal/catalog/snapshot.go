package catalog

import (
	"log/slog"
	"sort"
)

// snapshot is the in-memory view of the catalog, shared by YAMLStore and PGStore.
//
// Identity model: every resource has a Metadata.ID (UUIDv7, immutable PK) and a
// Metadata.Name (DNS-label slug, stable, mutable via id-routed PUT).
// Cross-references in spec fields use the slug — refs survive displayName edits
// trivially; refs are rewritten only on the rare slug-edit path. The primary
// in-memory maps are slug-keyed for the existing hot lookups; byID maps are
// secondary indexes (id → slug) maintained by buildByIDIndexes after any load
// or patch.
type snapshot struct {
	providers       map[string]*Provider
	models          map[string]*Model
	routes          map[string]*Route
	rateLimits      map[string]*RateLimit
	secrets         map[string]*Secret
	policies        map[string]*Policy
	relayKeys       map[string]*RelayKey // keyed by Metadata.Name (slug)
	relayKeysByHash map[string]*RelayKey // keyed by Spec.KeyHash for hot-path auth lookup
	passthrough     *Passthrough         // singleton; nil before first load, then always non-nil
	effectivePrices map[string]*Pricing  // keyed by model slug; populated by buildEffectivePricing
	// secretTierRLs holds one auto-injected system_mirrored RateLimit per
	// secret that has a resolvable upstream tier (via Spec.Tier or its
	// provider's Spec.DefaultTier). Keyed by secret slug. Populated by
	// injectUpstreamTierRateLimits; consumed by rateLimitsForRequest and
	// exposed via listRateLimits so they appear at GET /control/ratelimits.
	secretTierRLs map[string]*RateLimit

	// byID secondary indexes: id → slug. Built by buildByIDIndexes; used by the
	// HTTP layer's id-routed PUT/DELETE and slug-or-id GET. Empty before the
	// first build; rebuilt from scratch on each call (cheap, catalog is small).
	providersByID  map[string]string
	modelsByID     map[string]string
	routesByID     map[string]string
	rateLimitsByID map[string]string
	secretsByID    map[string]string
	policiesByID   map[string]string
	relayKeysByID  map[string]string
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
		policies:        map[string]*Policy{},
		relayKeys:       map[string]*RelayKey{},
		relayKeysByHash: map[string]*RelayKey{},
		effectivePrices: map[string]*Pricing{},
		secretTierRLs:   map[string]*RateLimit{},
		providersByID:   map[string]string{},
		modelsByID:      map[string]string{},
		routesByID:      map[string]string{},
		rateLimitsByID:  map[string]string{},
		secretsByID:     map[string]string{},
		policiesByID:    map[string]string{},
		relayKeysByID:   map[string]string{},
	}
}

// buildByIDIndexes rebuilds the id→slug secondary indexes for every kind.
// Call after any load or patch that mutates the slug-keyed primary maps.
// Skips entries with empty Metadata.ID (legacy or in-flight rows).
func (s *snapshot) buildByIDIndexes() {
	s.providersByID = make(map[string]string, len(s.providers))
	for slug, p := range s.providers {
		if p.Metadata.ID != "" {
			s.providersByID[p.Metadata.ID] = slug
		}
	}
	s.modelsByID = make(map[string]string, len(s.models))
	for slug, m := range s.models {
		if m.Metadata.ID != "" {
			s.modelsByID[m.Metadata.ID] = slug
		}
	}
	s.routesByID = make(map[string]string, len(s.routes))
	for slug, r := range s.routes {
		if r.Metadata.ID != "" {
			s.routesByID[r.Metadata.ID] = slug
		}
	}
	s.rateLimitsByID = make(map[string]string, len(s.rateLimits))
	for slug, rl := range s.rateLimits {
		if rl.Metadata.ID != "" {
			s.rateLimitsByID[rl.Metadata.ID] = slug
		}
	}
	s.secretsByID = make(map[string]string, len(s.secrets))
	for slug, sec := range s.secrets {
		if sec.Metadata.ID != "" {
			s.secretsByID[sec.Metadata.ID] = slug
		}
	}
	s.policiesByID = make(map[string]string, len(s.policies))
	for slug, p := range s.policies {
		if p.Metadata.ID != "" {
			s.policiesByID[p.Metadata.ID] = slug
		}
	}
	s.relayKeysByID = make(map[string]string, len(s.relayKeys))
	for slug, k := range s.relayKeys {
		if k.Metadata.ID != "" {
			s.relayKeysByID[k.Metadata.ID] = slug
		}
	}
}

// ── ByID accessors (secondary index) ──────────────────────────────────────────

func (s *snapshot) providerByID(id string) (*Provider, bool) {
	if slug, ok := s.providersByID[id]; ok {
		return s.providerByName(slug)
	}
	return nil, false
}

func (s *snapshot) modelByID(id string) (*Model, bool) {
	if slug, ok := s.modelsByID[id]; ok {
		return s.modelByName(slug)
	}
	return nil, false
}

func (s *snapshot) routeByID(id string) (*Route, bool) {
	if slug, ok := s.routesByID[id]; ok {
		return s.routeByName(slug)
	}
	return nil, false
}

func (s *snapshot) rateLimitByID(id string) (*RateLimit, bool) {
	if slug, ok := s.rateLimitsByID[id]; ok {
		return s.rateLimitByName(slug)
	}
	return nil, false
}

func (s *snapshot) secretByID(id string) (*Secret, bool) {
	if slug, ok := s.secretsByID[id]; ok {
		return s.secretByName(slug)
	}
	return nil, false
}

func (s *snapshot) policyByID(id string) (*Policy, bool) {
	if slug, ok := s.policiesByID[id]; ok {
		return s.policyByName(slug)
	}
	return nil, false
}

func (s *snapshot) relayKeyByID(id string) (*RelayKey, bool) {
	if slug, ok := s.relayKeysByID[id]; ok {
		return s.relayKeyByName(slug)
	}
	return nil, false
}

// rebuildRelayKeyHashIndex repopulates relayKeysByHash from relayKeys.
// Called after a snapshot mutation so hot-path auth lookups stay consistent.
func (s *snapshot) rebuildRelayKeyHashIndex() {
	s.relayKeysByHash = make(map[string]*RelayKey, len(s.relayKeys))
	for _, k := range s.relayKeys {
		if k.Spec.KeyHash == "" {
			continue
		}
		s.relayKeysByHash[k.Spec.KeyHash] = k
	}
}

func (s *snapshot) relayKeyByName(name string) (*RelayKey, bool) {
	k, ok := s.relayKeys[name]
	return k, ok
}

// relayKeyByHash returns the RelayKey whose Spec.KeyHash matches.
// Disabled or revoked keys ARE returned — callers must check IsEnabled and RevokedAt.
// Hot-path auth is the primary caller.
func (s *snapshot) relayKeyByHash(hash string) (*RelayKey, bool) {
	k, ok := s.relayKeysByHash[hash]
	return k, ok
}

// passthroughOrDefault returns the singleton or DefaultPassthrough() when unset.
func (s *snapshot) passthroughOrDefault() *Passthrough {
	if s.passthrough != nil {
		return s.passthrough
	}
	return DefaultPassthrough()
}

func (s *snapshot) listRelayKeys() []*RelayKey {
	out := make([]*RelayKey, 0, len(s.relayKeys))
	for _, k := range s.relayKeys {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
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
	if ep, ok := s.effectivePrices[modelName]; ok {
		return ep, true
	}
	if m, ok := s.modelByName(modelName); ok {
		ep, ok := s.effectivePrices[m.Metadata.Name]
		return ep, ok
	}
	return nil, false
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

func (s *snapshot) policyByName(name string) (*Policy, bool) {
	p, ok := s.policies[name]
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

func (s *snapshot) listPolicies() []*Policy {
	out := make([]*Policy, 0, len(s.policies))
	for _, p := range s.policies {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
}

func (s *snapshot) secretsForPolicy(p *Policy) []*Secret {
	seen := map[string]struct{}{}
	var out []*Secret
	for _, name := range p.Spec.Secrets {
		sec, ok := s.secrets[name]
		if !ok || !IsEnabled(sec.Spec.Enabled) {
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
			if !IsEnabled(sec.Spec.Enabled) {
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

// injectUpstreamTierRateLimits walks all secrets and, for each one that has a
// resolvable upstream tier (Spec.Tier or provider's Spec.DefaultTier), generates
// a synthetic system_mirrored RateLimit and stores it in s.secretTierRLs.
//
// The generated objects are also inserted into s.rateLimits so they appear in
// GET /control/ratelimits. The existing system_mirrored guard in the admin
// handlers blocks PUT/DELETE on them automatically via Spec.Source.
//
// Must be called after providers and secrets are fully populated.
func (s *snapshot) injectUpstreamTierRateLimits() {
	s.secretTierRLs = make(map[string]*RateLimit, len(s.secrets))
	for secretName, sec := range s.secrets {
		// Resolve effective tier: secret-level wins over provider default.
		tierName := sec.Spec.Tier
		if tierName == "" {
			if prov, ok := s.providers[sec.Spec.Provider]; ok {
				tierName = prov.Spec.DefaultTier
			}
		}
		if tierName == "" {
			continue // no tier declared; silent skip
		}
		tier := lookupUpstreamTier(tierName)
		if tier == nil {
			slog.Warn("upstream tier unknown; skipping auto-injection",
				"secret", secretName, "tier", tierName)
			continue
		}
		rlName := "upstream-" + secretName + "-" + tierName
		enabled := true
		rl := &RateLimit{
			APIVersion: APIVersion,
			Kind:       KindRateLimit,
			Metadata:   Metadata{Name: rlName},
			Spec: RateLimitSpec{
				Source:      string(SourceSystemMirrored),
				Strategy:    StrategySlidingWindow,
				Window:      0, // each rule carries its own window
				Rules:       tier.copyRules(),
				Enabled:     &enabled,
				Description: "Auto-injected upstream tier mirror for secret " + secretName,
			},
		}
		s.secretTierRLs[secretName] = rl
		// Also expose via rateLimits map for admin list/read endpoints.
		// Do not overwrite if an operator has manually defined a same-named RL.
		if _, exists := s.rateLimits[rlName]; !exists {
			s.rateLimits[rlName] = rl
		}
	}
}

func (s *snapshot) rateLimitsForRequest(provider *Provider, policy *Policy, model *Model, secret *Secret) []ResolvedRule {
	var out []ResolvedRule

	expand := func(parentKind Kind, parentName string, attachments []RateLimitAttachment) {
		for _, a := range attachments {
			rl, ok := s.rateLimits[a.Ref]
			if !ok || !IsEnabled(rl.Spec.Enabled) {
				continue
			}
			for _, rule := range rl.Spec.NormalizedRules() {
				strategy := rule.Strategy
				if strategy == "" {
					// Default per-rule strategy is token-bucket.
					strategy = StrategyTokenBucket
				}
				// Rule-level window overrides spec-level window; fall back to spec.Window.
				w := rule.Window
				if w == 0 {
					w = rl.Spec.Window
				}
				out = append(out, ResolvedRule{
					ParentKind:    parentKind,
					ParentName:    parentName,
					RateLimitName: rl.Metadata.Name,
					Strategy:      strategy,
					Window:        w,
					Rule:          rule,
					RateLimit:     rl,
					Meter:         Meter(rule.Meter),
				})
			}
		}
	}

	if secret != nil {
		expand(KindSecret, secret.Metadata.Name, secret.Spec.RateLimits)
		// Auto-injected upstream-tier RL for this secret (if any).
		if tierRL, ok := s.secretTierRLs[secret.Metadata.Name]; ok {
			expand(KindSecret, secret.Metadata.Name, []RateLimitAttachment{{Ref: tierRL.Metadata.Name}})
		}
	}
	if policy != nil {
		expand(KindPolicy, policy.Metadata.Name, policy.Spec.RateLimits)
	}
	if model != nil {
		expand(KindModel, model.Metadata.Name, model.Spec.RateLimits)
	}
	return out
}

func (s *snapshot) defaultProvider() *Provider {
	for _, p := range s.providers {
		if p.Spec.Default && IsEnabled(p.Spec.Enabled) {
			return p
		}
	}
	return nil
}

func (s *snapshot) defaultRoute() *Route {
	for _, r := range s.routes {
		if r.Spec.Default && IsEnabled(r.Spec.Enabled) {
			return r
		}
	}
	return nil
}

func (s *snapshot) providerForModel(modelName string) (*Provider, bool) {
	m, ok := s.modelByName(modelName)
	if !ok {
		return nil, false
	}
	p, ok := s.providers[m.Spec.Provider]
	return p, ok
}
