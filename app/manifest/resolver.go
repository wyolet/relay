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
	PricingID(name string) (string, bool)
	BindingID(name string) (string, bool)
	TeamID(name string) (string, bool)
	ProjectID(name string) (string, bool)
	ServiceAccountID(name string) (string, bool)
	GroupID(name string) (string, bool)
	RoleID(name string) (string, bool)
	UserID(username string) (string, bool)
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
	PricingName(id string) (string, bool)
	BindingName(id string) (string, bool)
	TeamName(id string) (string, bool)
	ProjectName(id string) (string, bool)
	ServiceAccountName(id string) (string, bool)
	GroupName(id string) (string, bool)
	RoleName(id string) (string, bool)
	Username(id string) (string, bool)
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
	Pricings   map[string]string
	Bindings   map[string]string
	Teams      map[string]string
	Projects   map[string]string

	ServiceAccounts map[string]string
	Groups          map[string]string
	Roles           map[string]string
	Users           map[string]string
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
func (m MapResolver) PricingID(name string) (string, bool) { v, ok := m.Pricings[name]; return v, ok }
func (m MapResolver) BindingID(name string) (string, bool) { v, ok := m.Bindings[name]; return v, ok }
func (m MapResolver) TeamID(name string) (string, bool)    { v, ok := m.Teams[name]; return v, ok }
func (m MapResolver) ProjectID(name string) (string, bool) { v, ok := m.Projects[name]; return v, ok }
func (m MapResolver) ServiceAccountID(name string) (string, bool) {
	v, ok := m.ServiceAccounts[name]
	return v, ok
}
func (m MapResolver) GroupID(name string) (string, bool) { v, ok := m.Groups[name]; return v, ok }
func (m MapResolver) RoleID(name string) (string, bool)  { v, ok := m.Roles[name]; return v, ok }
func (m MapResolver) UserID(name string) (string, bool)  { v, ok := m.Users[name]; return v, ok }

// MapReverseResolver is a convenience implementation of ReverseResolver backed
// by plain maps.
type MapReverseResolver struct {
	Providers  map[string]string
	Hosts      map[string]string
	Policies   map[string]string
	Models     map[string]string
	HostKeys   map[string]string
	RateLimits map[string]string
	Pricings   map[string]string
	Bindings   map[string]string
	Teams      map[string]string
	Projects   map[string]string

	ServiceAccounts map[string]string
	Groups          map[string]string
	Roles           map[string]string
	Users           map[string]string
	// ModelProviders maps modelID -> providerID, letting FromPolicy emit the
	// provider-qualified "provider/model" ref for legacy ModelIDs grants
	// (a bare modelref token means "provider", so a bare model slug would
	// re-import as the wrong grant). Optional; unset falls back to bare name.
	ModelProviders map[string]string
}

func (m MapReverseResolver) ModelProviderID(modelID string) (string, bool) {
	v, ok := m.ModelProviders[modelID]
	return v, ok
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
func (m MapReverseResolver) PricingName(id string) (string, bool) {
	v, ok := m.Pricings[id]
	return v, ok
}
func (m MapReverseResolver) BindingName(id string) (string, bool) {
	v, ok := m.Bindings[id]
	return v, ok
}
func (m MapReverseResolver) TeamName(id string) (string, bool) { v, ok := m.Teams[id]; return v, ok }
func (m MapReverseResolver) ProjectName(id string) (string, bool) {
	v, ok := m.Projects[id]
	return v, ok
}
func (m MapReverseResolver) ServiceAccountName(id string) (string, bool) {
	v, ok := m.ServiceAccounts[id]
	return v, ok
}
func (m MapReverseResolver) GroupName(id string) (string, bool) { v, ok := m.Groups[id]; return v, ok }
func (m MapReverseResolver) RoleName(id string) (string, bool)  { v, ok := m.Roles[id]; return v, ok }
func (m MapReverseResolver) Username(id string) (string, bool)  { v, ok := m.Users[id]; return v, ok }
