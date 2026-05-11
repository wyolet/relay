package catalog

// MemStore is a read-only Store backed by an in-memory snapshot.
// It is intended for tests and benchmarks that need a lightweight catalog
// without loading YAML from disk or connecting to Postgres.
type MemStore struct {
	snap *snapshot
}

// NewMemStore builds a MemStore from the supplied catalog objects.
// Only *Provider, *Secret, *Policy, *Model, *Route, and *RateLimit are
// recognised; anything else is silently ignored.
func NewMemStore(objects ...any) *MemStore {
	snap := newSnapshot()
	for _, obj := range objects {
		switch v := obj.(type) {
		case *Provider:
			snap.providers[v.Metadata.Name] = v
		case *Secret:
			snap.secrets[v.Metadata.Name] = v
		case *Policy:
			snap.policies[v.Metadata.Name] = v
		case *Model:
			snap.models[v.Metadata.Name] = v
		case *Route:
			snap.routes[v.Metadata.Name] = v
		case *RateLimit:
			snap.rateLimits[v.Metadata.Name] = v
		}
	}
	snap.buildEffectivePricing()
	return &MemStore{snap: snap}
}

func (m *MemStore) ProviderByName(name string) (*Provider, bool)   { return m.snap.providerByName(name) }
func (m *MemStore) ModelByName(name string) (*Model, bool)          { return m.snap.modelByName(name) }
func (m *MemStore) RouteByName(name string) (*Route, bool)          { return m.snap.routeByName(name) }
func (m *MemStore) RateLimitByName(name string) (*RateLimit, bool)  { return m.snap.rateLimitByName(name) }
func (m *MemStore) SecretByName(name string) (*Secret, bool)        { return m.snap.secretByName(name) }
func (m *MemStore) PolicyByName(name string) (*Policy, bool)            { return m.snap.policyByName(name) }
func (m *MemStore) Providers() []*Provider                          { return m.snap.listProviders() }
func (m *MemStore) Models() []*Model                                { return m.snap.listModels() }
func (m *MemStore) Routes() []*Route                                { return m.snap.listRoutes() }
func (m *MemStore) RateLimits() []*RateLimit                        { return m.snap.listRateLimits() }
func (m *MemStore) Secrets() []*Secret                              { return m.snap.listSecrets() }
func (m *MemStore) Policies() []*Policy                                  { return m.snap.listPolicies() }
func (m *MemStore) DefaultProvider() *Provider                      { return m.snap.defaultProvider() }
func (m *MemStore) DefaultRoute() *Route                            { return m.snap.defaultRoute() }
func (m *MemStore) ProviderForModel(modelName string) (*Provider, bool) {
	return m.snap.providerForModel(modelName)
}
func (m *MemStore) SecretsForPolicy(p *Policy) []*Secret { return m.snap.secretsForPolicy(p) }
func (m *MemStore) RateLimitsForRequest(provider *Provider, policy *Policy, model *Model, secret *Secret) []ResolvedRule {
	return m.snap.rateLimitsForRequest(provider, policy, model, secret)
}
func (m *MemStore) EffectivePricing(modelName string) (*Pricing, bool) {
	return m.snap.effectivePricingByModel(modelName)
}
