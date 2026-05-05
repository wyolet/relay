package configstore

type ConfigStore interface {
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
}
