package catalog

// Patch describes a prospective in-memory mutation to the catalog snapshot.
// Each field is optional; nil means "no change for this kind".
// Used by the admin CRUD factory to validate proposed mutations before committing.
type Patch struct {
	// UpsertProvider inserts or replaces the named provider.
	UpsertProvider *Provider
	// DeleteProvider removes the named provider.
	DeleteProvider string

	// UpsertModel inserts or replaces the named model.
	UpsertModel *Model
	// DeleteModel removes the named model.
	DeleteModel string

	// UpsertRoute inserts or replaces the named route.
	UpsertRoute *Route
	// DeleteRoute removes the named route.
	DeleteRoute string

	// UpsertRateLimit inserts or replaces the named rate limit.
	UpsertRateLimit *RateLimit
	// DeleteRateLimit removes the named rate limit.
	DeleteRateLimit string

	// UpsertSecret inserts or replaces the named secret.
	UpsertSecret *Secret
	// DeleteSecret removes the named secret.
	DeleteSecret string

	// UpsertPool inserts or replaces the named pool.
	UpsertPool *Pool
	// DeletePool removes the named pool.
	DeletePool string
}

// ValidateWithPatch clones the current in-memory snapshot, applies patch, and
// runs the catalog validator. Returns a validation error if the proposed state
// is invalid. Does not modify the live snapshot.
func (s *PGStore) ValidateWithPatch(patch Patch) error {
	base := s.cur()
	sim := cloneSnapshot(base)
	applyPatch(sim, patch)
	return validate(sim)
}

func cloneSnapshot(src *snapshot) *snapshot {
	dst := &snapshot{
		providers:  make(map[string]*Provider, len(src.providers)),
		models:     make(map[string]*Model, len(src.models)),
		routes:     make(map[string]*Route, len(src.routes)),
		rateLimits: make(map[string]*RateLimit, len(src.rateLimits)),
		secrets:    make(map[string]*Secret, len(src.secrets)),
		pools:      make(map[string]*Pool, len(src.pools)),
	}
	for k, v := range src.providers {
		dst.providers[k] = v
	}
	for k, v := range src.models {
		dst.models[k] = v
	}
	for k, v := range src.routes {
		dst.routes[k] = v
	}
	for k, v := range src.rateLimits {
		dst.rateLimits[k] = v
	}
	for k, v := range src.secrets {
		dst.secrets[k] = v
	}
	for k, v := range src.pools {
		dst.pools[k] = v
	}
	return dst
}

func applyPatch(s *snapshot, p Patch) {
	if p.UpsertProvider != nil {
		s.providers[p.UpsertProvider.Metadata.Name] = p.UpsertProvider
	}
	if p.DeleteProvider != "" {
		delete(s.providers, p.DeleteProvider)
	}
	if p.UpsertModel != nil {
		s.models[p.UpsertModel.Metadata.Name] = p.UpsertModel
	}
	if p.DeleteModel != "" {
		delete(s.models, p.DeleteModel)
	}
	if p.UpsertRoute != nil {
		s.routes[p.UpsertRoute.Metadata.Name] = p.UpsertRoute
	}
	if p.DeleteRoute != "" {
		delete(s.routes, p.DeleteRoute)
	}
	if p.UpsertRateLimit != nil {
		s.rateLimits[p.UpsertRateLimit.Metadata.Name] = p.UpsertRateLimit
	}
	if p.DeleteRateLimit != "" {
		delete(s.rateLimits, p.DeleteRateLimit)
	}
	if p.UpsertSecret != nil {
		s.secrets[p.UpsertSecret.Metadata.Name] = p.UpsertSecret
	}
	if p.DeleteSecret != "" {
		delete(s.secrets, p.DeleteSecret)
	}
	if p.UpsertPool != nil {
		s.pools[p.UpsertPool.Metadata.Name] = p.UpsertPool
	}
	if p.DeletePool != "" {
		delete(s.pools, p.DeletePool)
	}
}
