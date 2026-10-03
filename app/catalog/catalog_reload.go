package catalog

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/overlay"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
)

// ReloadTokenVersions rebuilds only the token-version map — the whole
// snapshot reaction to a users write.
func (c *Catalog) ReloadTokenVersions(ctx context.Context) error {
	if c.tokenVersions == nil {
		return nil
	}
	// The read is inside the lock: a full Reload landing between a read
	// outside it and the swap below would be overwritten by versions older
	// than the ones it just published.
	c.rmu.Lock()
	defer c.rmu.Unlock()
	versions, err := c.tokenVersions.TokenVersions(ctx)
	if err != nil {
		return fmt.Errorf("catalog: token versions: %w", err)
	}
	s := c.snap.Load().clone()
	s.tokenVersionByUser = versions
	s.usersLoaded = true
	c.snap.Store(s)
	return nil
}

// Reload reads every store, filters to enabled rows, runs cross-entity
// validation, builds a fresh Snapshot, and atomic-swaps it in. On any
// error the existing Snapshot stays live — callers can retry.
func (c *Catalog) Reload(ctx context.Context) error {
	// Serialize with the COW reconciler (and other reloads): every Apply*
	// does clone→mutate→Store under rmu; publishing here without it lets a
	// concurrent Apply clone the pre-reload snapshot and clobber this one.
	c.rmu.Lock()
	defer c.rmu.Unlock()
	return c.reloadLocked(ctx)
}

// reloadLocked is Reload's body. Caller must hold c.rmu — Apply* uses it
// directly to recover from an absent-id upsert without re-locking.
func (c *Catalog) reloadLocked(ctx context.Context) error {
	provs, err := c.providers.List(ctx)
	if err != nil {
		return fmt.Errorf("catalog reload: providers: %w", err)
	}
	hosts, err := c.hosts.List(ctx)
	if err != nil {
		return fmt.Errorf("catalog reload: hosts: %w", err)
	}
	pols, err := c.policies.List(ctx)
	if err != nil {
		return fmt.Errorf("catalog reload: policies: %w", err)
	}
	models, err := c.models.List(ctx)
	if err != nil {
		return fmt.Errorf("catalog reload: models: %w", err)
	}
	hostKeys, err := c.hostKeys.List(ctx)
	if err != nil {
		return fmt.Errorf("catalog reload: providerkeys: %w", err)
	}
	rls, err := c.rateLimits.List(ctx)
	if err != nil {
		return fmt.Errorf("catalog reload: ratelimits: %w", err)
	}
	rks, err := c.keys.List(ctx)
	if err != nil {
		return fmt.Errorf("catalog reload: keys: %w", err)
	}
	pricingsAll, err := c.pricings.List(ctx)
	if err != nil {
		return fmt.Errorf("catalog reload: pricings: %w", err)
	}
	bindingsAll, err := c.bindings.List(ctx)
	if err != nil {
		return fmt.Errorf("catalog reload: bindings: %w", err)
	}
	var teams []*team.Team
	if c.teams != nil {
		teams, err = c.teams.List(ctx)
		if err != nil {
			return fmt.Errorf("catalog reload: teams: %w", err)
		}
	}
	var projects []*project.Project
	if c.projects != nil {
		projects, err = c.projects.List(ctx)
		if err != nil {
			return fmt.Errorf("catalog reload: projects: %w", err)
		}
	}
	var sas []*serviceaccount.ServiceAccount
	if c.serviceAccounts != nil {
		sas, err = c.serviceAccounts.List(ctx)
		if err != nil {
			return fmt.Errorf("catalog reload: service accounts: %w", err)
		}
	}
	var groups []*group.Group
	if c.groups != nil {
		groups, err = c.groups.List(ctx)
		if err != nil {
			return fmt.Errorf("catalog reload: groups: %w", err)
		}
	}
	var roles []*role.Role
	if c.roles != nil {
		roles, err = c.roles.List(ctx)
		if err != nil {
			return fmt.Errorf("catalog reload: roles: %w", err)
		}
	}
	var roleBindings []*rolebinding.RoleBinding
	if c.roleBindings != nil {
		roleBindings, err = c.roleBindings.List(ctx)
		if err != nil {
			return fmt.Errorf("catalog reload: role bindings: %w", err)
		}
	}
	var policyBindings []*policybinding.PolicyBinding
	if c.policyBindings != nil {
		policyBindings, err = c.policyBindings.List(ctx)
		if err != nil {
			return fmt.Errorf("catalog reload: policy bindings: %w", err)
		}
	}
	var tokenVersions map[string]int
	if c.tokenVersions != nil {
		tokenVersions, err = c.tokenVersions.TokenVersions(ctx)
		if err != nil {
			return fmt.Errorf("catalog reload: token versions: %w", err)
		}
	}
	var ovls []*overlay.Overlay
	if c.overlays != nil {
		ovls, err = c.overlays.List(ctx)
		if err != nil {
			return fmt.Errorf("catalog reload: overlays: %w", err)
		}
	}

	enabledProvs := filter(provs, (*provider.Provider).IsEnabled)
	enabledHosts := filter(hosts, (*host.Host).IsEnabled)
	enabledRKs := filter(rks, (*key.Key).IsEnabled)
	enabledModels := filter(models, (*model.Model).IsEnabled)
	enabledKeys := filter(hostKeys, (*hostkey.HostKey).IsEnabled)
	enabledRLs := filter(rls, (*ratelimit.RateLimit).IsEnabled)
	enabledPricings := filter(pricingsAll, (*pricing.Pricing).IsEnabled)
	enabledBindings := filter(bindingsAll, (*binding.Binding).IsEnabled)
	enabledTeams := filter(teams, (*team.Team).IsEnabled)
	enabledProjects := filter(projects, (*project.Project).IsEnabled)
	enabledSAs := filter(sas, (*serviceaccount.ServiceAccount).IsEnabled)
	enabledGroups := filter(groups, (*group.Group).IsEnabled)
	enabledRoles := filter(roles, (*role.Role).IsEnabled)
	enabledRoleBindings := filter(roleBindings, (*rolebinding.RoleBinding).IsEnabled)
	enabledPolicyBindings := filter(policyBindings, (*policybinding.PolicyBinding).IsEnabled)

	providerIDs := make(map[string]struct{}, len(enabledProvs))
	for _, p := range enabledProvs {
		providerIDs[p.Meta.ID] = struct{}{}
	}
	hostIDs := make(map[string]struct{}, len(enabledHosts))
	for _, h := range enabledHosts {
		hostIDs[h.Meta.ID] = struct{}{}
	}

	if err := validateCross(providerIDs, hostIDs, enabledHosts, pols, enabledRKs, enabledModels, enabledKeys, enabledRLs, enabledPricings, enabledBindings); err != nil {
		return fmt.Errorf("catalog reload: %w", err)
	}

	snap := build(c.now, enabledProvs, enabledHosts, pols, enabledRKs, enabledModels, enabledKeys, enabledRLs, enabledPricings, enabledBindings, ovls, enabledTeams, enabledProjects, enabledSAs, enabledGroups,
		enabledRoles, enabledRoleBindings, enabledPolicyBindings)
	// The own-scope hash index covers disabled keys too, so it is built from
	// the unfiltered rows rather than the ones the snapshot routes on.
	for _, k := range rks {
		snap.indexUserKeyHashes(k)
	}
	if tokenVersions != nil {
		snap.tokenVersionByUser = tokenVersions
		snap.usersLoaded = true
	}
	c.snap.Store(snap)
	c.markReady()
	return nil
}

// filter never compacts in place: a Lister may hand back a shared slice
// (in-memory stores, test fixtures) and Apply-triggered rebuilds re-List it.
func filter[T any](items []T, keep func(T) bool) []T {
	out := make([]T, 0, len(items))
	for _, it := range items {
		if keep(it) {
			out = append(out, it)
		}
	}
	return out
}
