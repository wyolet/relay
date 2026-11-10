package manifest

import "fmt"

// RefNotFoundError is a name a document references that the Resolver does
// not know. Typed so a caller scoped to part of the deployment can be
// answered as for a row it may not see.
type RefNotFoundError struct{ msg string }

func (e *RefNotFoundError) Error() string { return e.msg }

// RefNotFound returns a RefNotFoundError with the formatted message.
func RefNotFound(format string, args ...any) error {
	return &RefNotFoundError{msg: fmt.Sprintf(format, args...)}
}

// Resolver resolves catalog-kind names to ids. The caller builds one from
// their name→id index (a snapshot, a seed index built against live PG state,
// etc.); translation never touches a full catalog or storage layer.
type Resolver interface {
	ProviderID(name string) (string, bool)
	HostID(name string) (string, bool)
	PolicyID(name string) (string, bool)
	ModelID(name string) (string, bool)
	RateLimitID(name string) (string, bool)
	PricingID(name string) (string, bool)
	BindingID(name string) (string, bool)
}

// MapResolver is a Resolver backed by plain name→id maps.
type MapResolver struct {
	Providers  map[string]string
	Hosts      map[string]string
	Policies   map[string]string
	Models     map[string]string
	RateLimits map[string]string
	Pricings   map[string]string
	Bindings   map[string]string
}

func (m MapResolver) ProviderID(name string) (string, bool) { v, ok := m.Providers[name]; return v, ok }
func (m MapResolver) HostID(name string) (string, bool)     { v, ok := m.Hosts[name]; return v, ok }
func (m MapResolver) PolicyID(name string) (string, bool)   { v, ok := m.Policies[name]; return v, ok }
func (m MapResolver) ModelID(name string) (string, bool)    { v, ok := m.Models[name]; return v, ok }
func (m MapResolver) RateLimitID(name string) (string, bool) {
	v, ok := m.RateLimits[name]
	return v, ok
}
func (m MapResolver) PricingID(name string) (string, bool) { v, ok := m.Pricings[name]; return v, ok }
func (m MapResolver) BindingID(name string) (string, bool) { v, ok := m.Bindings[name]; return v, ok }
