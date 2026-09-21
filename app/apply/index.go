package apply

import (
	"github.com/wyolet/relay/app/meta"
)

// index is a mutable name→id map per kind, populated from the existing rows
// plus the ids minted for names the manifest introduces. It satisfies
// manifest.Resolver directly, so translate resolves cross-refs against the
// post-mint state and forward references inside one apply resolve.
type index struct {
	Providers  map[string]string
	Hosts      map[string]string
	RateLimits map[string]string
	HostKeys   map[string]string
	Models     map[string]string
	Pricings   map[string]string
	Bindings   map[string]string
	Policies   map[string]string
	Keys       map[string]string
	Teams      map[string]string
	Projects   map[string]string

	ServiceAccounts map[string]string
	Groups          map[string]string
	Roles           map[string]string
	RoleBindings    map[string]string
	PolicyBindings  map[string]string
	Users           map[string]string
}

func newIndex(r *Rows) *index {
	i := &index{
		Providers:  map[string]string{},
		Hosts:      map[string]string{},
		RateLimits: map[string]string{},
		HostKeys:   map[string]string{},
		Models:     map[string]string{},
		Pricings:   map[string]string{},
		Bindings:   map[string]string{},
		Policies:   map[string]string{},
		Keys:       map[string]string{},
		Teams:      map[string]string{},
		Projects:   map[string]string{},

		ServiceAccounts: map[string]string{},
		Groups:          map[string]string{},
		Roles:           map[string]string{},
		RoleBindings:    map[string]string{},
		PolicyBindings:  map[string]string{},
		Users:           map[string]string{},
	}
	for _, t := range r.Teams {
		i.Teams[t.Meta.Name] = t.Meta.ID
	}
	for _, p := range r.Projects {
		i.Projects[p.Meta.Name] = p.Meta.ID
	}
	for _, p := range r.Providers {
		i.Providers[p.Meta.Name] = p.Meta.ID
	}
	for _, h := range r.Hosts {
		i.Hosts[h.Meta.Name] = h.Meta.ID
	}
	for _, rl := range r.RateLimits {
		i.RateLimits[rl.Meta.Name] = rl.Meta.ID
	}
	for _, k := range r.HostKeys {
		i.HostKeys[k.Meta.Name] = k.Meta.ID
	}
	for _, m := range r.Models {
		i.Models[m.Meta.Name] = m.Meta.ID
	}
	for _, p := range r.Pricings {
		i.Pricings[p.Meta.Name] = p.Meta.ID
	}
	for _, b := range r.Bindings {
		i.Bindings[b.Meta.Name] = b.Meta.ID
	}
	for _, p := range r.Policies {
		i.Policies[p.Meta.Name] = p.Meta.ID
	}
	for _, k := range r.Keys {
		i.Keys[k.Meta.Name] = k.Meta.ID
	}
	for _, sa := range r.ServiceAccounts {
		i.ServiceAccounts[sa.Meta.Name] = sa.Meta.ID
	}
	for _, g := range r.Groups {
		i.Groups[g.Meta.Name] = g.Meta.ID
	}
	for _, ro := range r.Roles {
		i.Roles[ro.Meta.Name] = ro.Meta.ID
	}
	for _, b := range r.RoleBindings {
		i.RoleBindings[b.Meta.Name] = b.Meta.ID
	}
	for _, b := range r.PolicyBindings {
		i.PolicyBindings[b.Meta.Name] = b.Meta.ID
	}
	for _, u := range r.Users {
		i.Users[u.Username] = u.ID
	}
	return i
}

func (i *index) ProviderID(n string) (string, bool)  { v, ok := i.Providers[n]; return v, ok }
func (i *index) HostID(n string) (string, bool)      { v, ok := i.Hosts[n]; return v, ok }
func (i *index) PolicyID(n string) (string, bool)    { v, ok := i.Policies[n]; return v, ok }
func (i *index) ModelID(n string) (string, bool)     { v, ok := i.Models[n]; return v, ok }
func (i *index) HostKeyID(n string) (string, bool)   { v, ok := i.HostKeys[n]; return v, ok }
func (i *index) RateLimitID(n string) (string, bool) { v, ok := i.RateLimits[n]; return v, ok }
func (i *index) PricingID(n string) (string, bool)   { v, ok := i.Pricings[n]; return v, ok }
func (i *index) BindingID(n string) (string, bool)   { v, ok := i.Bindings[n]; return v, ok }
func (i *index) TeamID(n string) (string, bool)      { v, ok := i.Teams[n]; return v, ok }
func (i *index) ProjectID(n string) (string, bool)   { v, ok := i.Projects[n]; return v, ok }
func (i *index) ServiceAccountID(n string) (string, bool) {
	v, ok := i.ServiceAccounts[n]
	return v, ok
}
func (i *index) GroupID(n string) (string, bool) { v, ok := i.Groups[n]; return v, ok }
func (i *index) RoleID(n string) (string, bool)  { v, ok := i.Roles[n]; return v, ok }
func (i *index) UserID(n string) (string, bool)  { v, ok := i.Users[n]; return v, ok }

// mintIDs assigns a fresh UUIDv7 to every manifest name the index doesn't
// know yet, before translate runs, so cross-refs resolve against names this
// same apply introduces.
func mintIDs[T any](into map[string]string, docs []T, name func(T) string) {
	for _, d := range docs {
		n := name(d)
		if _, ok := into[n]; !ok {
			into[n] = meta.NewID()
		}
	}
}
