// Snapshot assembly. build is intentionally a thin orchestrator: it
// computes the input-id sets and calls each kind's addX method in the
// order their dependencies require. The per-kind logic (sanitize +
// register) lives in build_<kind>.go.
//
// Cross-references in each entity are *sanitized* against the input
// enabled-id sets before insertion: a ref to a missing or disabled row
// is silently dropped from the snapshot copy. The full row stays in
// Postgres for the control plane. Reload never fails over a stale ref —
// the snapshot is always the consistent reachable subgraph.
package catalog

import (
	"time"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/overlay"
	"github.com/wyolet/relay/app/policy"
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

// Build assembles a Snapshot from entity slices using the same sanitize rules
// as Reload. Used by catalog-embed and tests; callers supply only the kinds
// they need — pass nil/empty slices for the rest.
func Build(
	provs []*provider.Provider,
	hosts []*host.Host,
	pols []*policy.Policy,
	rks []*key.Key,
	models []*model.Model,
	keys []*hostkey.HostKey,
	rls []*ratelimit.RateLimit,
	pricings []*pricing.Pricing,
	bindings []*binding.Binding,
) *Snapshot {
	return build(nil, provs, hosts, pols, rks, models, keys, rls, pricings, bindings, nil, nil, nil, nil, nil, nil, nil, nil)
}

func build(
	now func() time.Time,
	provs []*provider.Provider,
	hosts []*host.Host,
	pols []*policy.Policy,
	rks []*key.Key,
	models []*model.Model,
	keys []*hostkey.HostKey,
	rls []*ratelimit.RateLimit,
	pricings []*pricing.Pricing,
	bindings []*binding.Binding,
	ovls []*overlay.Overlay,
	teams []*team.Team,
	projects []*project.Project,
	sas []*serviceaccount.ServiceAccount,
	groups []*group.Group,
	roles []*role.Role,
	roleBindings []*rolebinding.RoleBinding,
	policyBindings []*policybinding.PolicyBinding,
) *Snapshot {
	s := newEmptySnapshot(len(provs), len(hosts), len(pols), len(rks), len(models), len(keys), len(rls), len(pricings), len(bindings), len(teams), len(projects), len(sas), len(groups), len(roles), len(roleBindings), len(policyBindings))
	s.now = now

	providerIDs := setFromIDs(provs, func(p *provider.Provider) string { return p.Meta.ID })
	hostIDs := setFromIDs(hosts, func(h *host.Host) string { return h.Meta.ID })
	polByID := make(map[string]*policy.Policy, len(pols))
	for _, p := range pols {
		polByID[p.Meta.ID] = p
	}
	polIDSet := snapIDs(polByID)

	s.addProviders(provs)
	// Tenancy first: every kind below can be project-owned and sanitizes
	// against the project map.
	s.addTeams(teams)
	s.addProjects(projects, snapIDs(s.teamsByID))
	s.addRateLimits(rls, snapIDs(s.projectsByID))
	s.addHosts(hosts, polByID)
	s.addModels(models, providerIDs)
	// Overlays swap templates for effective rows BEFORE indexing, so
	// aliases/refs below index the merged spec.
	s.applyOverlays(ovls)
	s.addHostKeys(keys, hostIDs, snapIDs(s.projectsByID), polByID)
	// Droppable kinds (models: missing provider; hostkeys: missing host /
	// tier policy) are in place now, so dependents sanitize against the
	// post-drop snapshot membership — the input enabled-id sets would keep
	// refs to rows the pass above dropped, diverging from the incremental
	// cascade's fixpoint.
	memberModelIDs := snapIDs(s.modelsByID)
	memberKeyIDs := snapIDs(s.hostKeysByID)
	s.addPolicies(pols, memberModelIDs, memberKeyIDs, snapIDs(s.rateLimitsByID), snapIDs(s.projectsByID))
	s.addGroups(groups)
	s.addRoles(roles)
	// Service accounts sanitize against the policies just indexed; keys
	// then sanitize against the accounts.
	s.addServiceAccounts(sas, snapIDs(s.projectsByID), snapIDs(s.policiesByID))
	s.addKeys(rks, polIDSet, snapIDs(s.serviceAccountsByID))
	s.computePolicyReverseJoins()
	s.addPricings(pricings, hostIDs, memberModelIDs)
	s.addBindings(bindings, memberModelIDs, hostIDs)
	// Aliases + policy allow-sets read bindings (BindingsForModel), so they
	// must run after bindings are indexed.
	for _, m := range s.modelsByID {
		s.indexModelSnapshots(m)
	}
	s.rebuildPolicyAllowSets()
	// Bindings last: they sanitize against the roles, tenancy rows and
	// policies already indexed above.
	s.addRoleBindings(roleBindings, snapIDs(s.rolesByID), snapIDs(s.teamsByID), snapIDs(s.projectsByID))
	s.addPolicyBindings(policyBindings, snapIDs(s.projectsByID), snapIDs(s.policiesByID))

	return s
}

func newEmptySnapshot(nProvs, nHosts, nPols, nRks, nModels, nKeys, nRLs, nPricings, nBindings, nTeams, nProjects, nSAs, nGroups, nRoles, nRoleBindings, nPolicyBindings int) *Snapshot {
	return &Snapshot{
		providersByID:         make(map[string]*provider.Provider, nProvs),
		providersByName:       make(map[string]*provider.Provider, nProvs),
		hostsByID:             make(map[string]*host.Host, nHosts),
		hostsByName:           make(map[string]*host.Host, nHosts),
		policiesByID:          make(map[string]*policy.Policy, nPols),
		policiesByName:        make(map[string]*policy.Policy, nPols),
		modelsByID:            make(map[string]*model.Model, nModels),
		modelsByName:          map[string][]*model.Model{},
		snapshotsByName:       map[string]snapshotRef{},
		snapshotAliases:       map[string]snapshotRef{},
		aliasExact:            map[string]AliasRef{},
		overlaysByTarget:      map[string]*overlay.Overlay{},
		modelTemplates:        map[string]*model.Model{},
		hostKeysByID:          make(map[string]*hostkey.HostKey, nKeys),
		rateLimitsByID:        make(map[string]*ratelimit.RateLimit, nRLs),
		rateLimitsByName:      make(map[string]*ratelimit.RateLimit, nRLs),
		keysByID:              make(map[string]*key.Key, nRks),
		keysByHash:            make(map[string]*key.Key, nRks),
		keysByPrincipal:       make(map[string][]*key.Key, nRks),
		subjectsByKey:         make(map[string][]string, nRks),
		tokenVersionByUser:    map[string]int{},
		modelsByPolicy:        map[string][]*model.Model{},
		hostKeysByPolicy:      map[string][]*hostkey.HostKey{},
		rateLimitByPolicy:     map[string]*ratelimit.RateLimit{},
		allowedCombosByPolicy: map[string]map[comboKey]struct{}{},
		pricingsByID:          make(map[string]*pricing.Pricing, nPricings),
		pricingByModelHost:    map[string]*pricing.Pricing{},
		bindingsByID:          make(map[string]*binding.Binding, nBindings),
		bindingsByModelHost:   make(map[string]*binding.Binding, nBindings),
		bindingsByModel:       map[string][]*binding.Binding{},
		refsByProvider:        map[string]refSet{},
		refsByHost:            map[string]refSet{},
		refsByModel:           map[string]refSet{},
		refsByHostKey:         map[string]refSet{},
		refsByRateLimit:       map[string]refSet{},
		refsByPolicy:          map[string]refSet{},
		refsByTeam:            map[string]refSet{},
		refsByProject:         map[string]refSet{},
		teamsByID:             make(map[string]*team.Team, nTeams),
		teamsByName:           make(map[string]*team.Team, nTeams),
		projectsByID:          make(map[string]*project.Project, nProjects),
		projectsByName:        make(map[string]*project.Project, nProjects),
		projectsByTeam:        map[string][]*project.Project{},
		refsByServiceAccount:  map[string]refSet{},
		refsByRole:            map[string]refSet{},

		serviceAccountsByID:      make(map[string]*serviceaccount.ServiceAccount, nSAs),
		serviceAccountsByName:    make(map[string]*serviceaccount.ServiceAccount, nSAs),
		serviceAccountsByProject: map[string][]*serviceaccount.ServiceAccount{},

		groupsByID:   make(map[string]*group.Group, nGroups),
		groupsByName: make(map[string]*group.Group, nGroups),
		groupsByUser: map[string][]string{},

		rolesByID:   make(map[string]*role.Role, nRoles),
		rolesByName: make(map[string]*role.Role, nRoles),

		roleBindingsByID:      make(map[string]*rolebinding.RoleBinding, nRoleBindings),
		roleBindingsBySubject: map[string][]*rolebinding.RoleBinding{},

		policyBindingsByID:      make(map[string]*policybinding.PolicyBinding, nPolicyBindings),
		policyBindingsByProject: map[string][]*policybinding.PolicyBinding{},
	}
}
