package catalog

type Store interface {
	ProviderByName(name string) (*Provider, bool)
	ModelByName(name string) (*Model, bool)
	RouteByName(name string) (*Route, bool)
	RateLimitByName(name string) (*RateLimit, bool)
	SecretByName(name string) (*Secret, bool)
	PolicyByName(name string) (*Policy, bool)
	// ByID accessors. Used by the id-routed admin CRUD path
	// (PUT/DELETE /control/{kind}/by-id/{id}) and by GET when the path
	// component parses as a UUID.
	ProviderByID(id string) (*Provider, bool)
	ModelByID(id string) (*Model, bool)
	RouteByID(id string) (*Route, bool)
	RateLimitByID(id string) (*RateLimit, bool)
	SecretByID(id string) (*Secret, bool)
	PolicyByID(id string) (*Policy, bool)
	RelayKeyByID(id string) (*RelayKey, bool)
	Providers() []*Provider
	Models() []*Model
	Routes() []*Route
	RateLimits() []*RateLimit
	Secrets() []*Secret
	Policies() []*Policy
	DefaultProvider() *Provider
	DefaultRoute() *Route
	ProviderForModel(modelName string) (*Provider, bool)
	SecretsForPolicy(p *Policy) []*Secret
	RateLimitsForRequest(provider *Provider, policy *Policy, model *Model, secret *Secret) []ResolvedRule
	// EffectivePricing returns the merged pricing for a model (provider default +
	// model-level overlay). Returns nil, false when no pricing is configured.
	EffectivePricing(modelName string) (*Pricing, bool)

	RelayKeyByName(name string) (*RelayKey, bool)
	// RelayKeyByHash returns the RelayKey whose Spec.KeyHash matches the given
	// hex string. Hot-path auth lookup. The returned key MAY be revoked or
	// disabled — callers must check.
	RelayKeyByHash(hash string) (*RelayKey, bool)
	RelayKeys() []*RelayKey

	// Passthrough returns the singleton config or DefaultPassthrough() when unset.
	Passthrough() *Passthrough
}
