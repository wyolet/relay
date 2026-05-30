package manifest

// Resolver resolves entity names to ids. The caller builds one from their
// name→id index (a snapshot, a seed index built against live PG state, etc.).
// Wire needs only this narrow interface — it never touches a full catalog or
// storage layer.
type Resolver interface {
	ProviderID(name string) (string, bool)
	HostID(name string) (string, bool)
	PolicyID(name string) (string, bool)
	ModelID(name string) (string, bool)
	HostKeyID(name string) (string, bool)
	RateLimitID(name string) (string, bool)
}

// ReverseResolver resolves entity ids to names. Used to render domain structs
// back to wire DTOs for admin GET responses.
type ReverseResolver interface {
	ProviderName(id string) (string, bool)
	HostName(id string) (string, bool)
	PolicyName(id string) (string, bool)
	ModelName(id string) (string, bool)
	HostKeyName(id string) (string, bool)
	RateLimitName(id string) (string, bool)
}

// MapResolver is a convenience implementation of Resolver backed by plain
// maps. Useful in tests and seed tooling.
type MapResolver struct {
	Providers  map[string]string
	Hosts      map[string]string
	Policies   map[string]string
	Models     map[string]string
	HostKeys   map[string]string
	RateLimits map[string]string
}

func (m MapResolver) ProviderID(name string) (string, bool) { v, ok := m.Providers[name]; return v, ok }
func (m MapResolver) HostID(name string) (string, bool)     { v, ok := m.Hosts[name]; return v, ok }
func (m MapResolver) PolicyID(name string) (string, bool)   { v, ok := m.Policies[name]; return v, ok }
func (m MapResolver) ModelID(name string) (string, bool)    { v, ok := m.Models[name]; return v, ok }
func (m MapResolver) HostKeyID(name string) (string, bool)  { v, ok := m.HostKeys[name]; return v, ok }
func (m MapResolver) RateLimitID(name string) (string, bool) {
	v, ok := m.RateLimits[name]
	return v, ok
}

// MapReverseResolver is a convenience implementation of ReverseResolver backed
// by plain maps.
type MapReverseResolver struct {
	Providers  map[string]string
	Hosts      map[string]string
	Policies   map[string]string
	Models     map[string]string
	HostKeys   map[string]string
	RateLimits map[string]string
}

func (m MapReverseResolver) ProviderName(id string) (string, bool) {
	v, ok := m.Providers[id]
	return v, ok
}
func (m MapReverseResolver) HostName(id string) (string, bool) { v, ok := m.Hosts[id]; return v, ok }
func (m MapReverseResolver) PolicyName(id string) (string, bool) {
	v, ok := m.Policies[id]
	return v, ok
}
func (m MapReverseResolver) ModelName(id string) (string, bool) { v, ok := m.Models[id]; return v, ok }
func (m MapReverseResolver) HostKeyName(id string) (string, bool) {
	v, ok := m.HostKeys[id]
	return v, ok
}
func (m MapReverseResolver) RateLimitName(id string) (string, bool) {
	v, ok := m.RateLimits[id]
	return v, ok
}
