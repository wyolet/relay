package catalog

type Store interface {
	ProviderByName(name string) (*Provider, bool)
	ModelByName(name string) (*Model, bool)
	RouteByName(name string) (*Route, bool)
	RateLimitByName(name string) (*RateLimit, bool)
	SecretByName(name string) (*Secret, bool)
	PoolByName(name string) (*Pool, bool)
	Providers() []*Provider
	Models() []*Model
	Routes() []*Route
	RateLimits() []*RateLimit
	Secrets() []*Secret
	Pools() []*Pool
	DefaultProvider() *Provider
	DefaultRoute() *Route
	ProviderForModel(modelName string) (*Provider, bool)
	SecretsForPool(p *Pool) []*Secret
	RateLimitsForRequest(provider *Provider, pool *Pool, model *Model, secret *Secret) []ResolvedRule
	// EffectivePricing returns the merged pricing for a model (provider default +
	// model-level overlay). Returns nil, false when no pricing is configured.
	EffectivePricing(modelName string) (*Pricing, bool)
}
