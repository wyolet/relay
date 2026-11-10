package control

import (
	"github.com/wyolet/relay/app/apply"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/pkg/ids"
)

// nameOwner rewrites an owner reference from the stored id to the slug the
// manifest names it by, so a bundle in git reads as slugs, not UUIDs.
func (e *exporter) nameOwner(m *manifest.WireMeta) {
	if m == nil || !ids.Valid(m.Owner.Name) {
		return
	}
	var (
		name string
		ok   bool
	)
	switch m.Owner.Kind {
	case meta.OwnerTeam:
		name, ok = e.rev.TeamName(m.Owner.Name)
	case meta.OwnerProject:
		name, ok = e.rev.ProjectName(m.Owner.Name)
	case meta.OwnerUser:
		name, ok = e.rev.Username(m.Owner.Name)
	case meta.OwnerHost:
		name, ok = e.rev.HostName(m.Owner.Name)
	}
	if ok {
		m.Owner.Name = name
	}
}

// reverseResolver builds the id→name index the From* renderers need.
func reverseResolver(r *apply.Rows) manifest.MapReverseResolver {
	rev := manifest.MapReverseResolver{
		Providers: map[string]string{}, Hosts: map[string]string{},
		Policies: map[string]string{}, Models: map[string]string{},
		HostKeys: map[string]string{}, RateLimits: map[string]string{},
		Pricings: map[string]string{}, Bindings: map[string]string{},
		Teams: map[string]string{}, Projects: map[string]string{},
		ServiceAccounts: map[string]string{}, Groups: map[string]string{},
		Roles: map[string]string{}, Users: map[string]string{},
		Accounts: map[string]string{}, Pools: map[string]string{},
		Budgets: map[string]string{},
	}
	for _, x := range r.Accounts {
		rev.Accounts[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.Pools {
		rev.Pools[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.Budgets {
		rev.Budgets[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.Providers {
		rev.Providers[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.Hosts {
		rev.Hosts[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.Policies {
		rev.Policies[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.Models {
		rev.Models[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.HostKeys {
		rev.HostKeys[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.RateLimits {
		rev.RateLimits[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.Pricings {
		rev.Pricings[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.Bindings {
		rev.Bindings[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.Teams {
		rev.Teams[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.Projects {
		rev.Projects[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.ServiceAccounts {
		rev.ServiceAccounts[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.Groups {
		rev.Groups[x.Meta.ID] = x.Meta.Name
	}
	for _, x := range r.Roles {
		rev.Roles[x.Meta.ID] = x.Meta.Name
	}
	for _, u := range r.Users {
		rev.Users[u.ID] = u.Username
	}
	return rev
}
