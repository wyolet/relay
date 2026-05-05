package configstore

type ConfigStore interface {
	ProviderByName(name string) (*Provider, bool)
	ModelByName(name string) (*Model, bool)
	RouteByName(name string) (*Route, bool)
	RateLimitByName(name string) (*RateLimit, bool)
	Providers() []*Provider
	Models() []*Model
	Routes() []*Route
	RateLimits() []*RateLimit
	DefaultProvider() *Provider
	DefaultRoute() *Route
	ProviderForModel(modelName string) (*Provider, bool)
}
