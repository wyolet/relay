package catalog

import "github.com/wyolet/relay/app/provider"

// Providers carry no cross-refs in v1, so there's nothing to sanitize.
func (s *Snapshot) addProviders(provs []*provider.Provider) {
	for _, p := range provs {
		s.providersByID[p.Meta.ID] = p
		s.providersByName[p.Meta.Name] = p
	}
}
