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
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
)

// Build assembles a Snapshot from entity slices using the same sanitize rules
// as Reload. Used by catalog-embed and tests; callers supply only the kinds
// they need — pass nil/empty slices for the rest.
func Build(
	provs []*provider.Provider,
	hosts []*host.Host,
	pols []*policy.Policy,
	rks []*relaykey.RelayKey,
	models []*model.Model,
	keys []*hostkey.HostKey,
	rls []*ratelimit.RateLimit,
	pricings []*pricing.Pricing,
	bindings []*binding.Binding,
) *Snapshot {
	return build(provs, hosts, pols, rks, models, keys, rls, pricings, bindings)
}

func build(
	provs []*provider.Provider,
	hosts []*host.Host,
	pols []*policy.Policy,
	rks []*relaykey.RelayKey,
	models []*model.Model,
	keys []*hostkey.HostKey,
	rls []*ratelimit.RateLimit,
	pricings []*pricing.Pricing,
	bindings []*binding.Binding,
) *Snapshot {
	s := newEmptySnapshot(len(provs), len(hosts), len(pols), len(rks), len(models), len(keys), len(rls), len(pricings), len(bindings))

	providerIDs := setFromIDs(provs, func(p *provider.Provider) string { return p.Meta.ID })
	hostIDs := setFromIDs(hosts, func(h *host.Host) string { return h.Meta.ID })
	modelIDs := setFromIDs(models, func(m *model.Model) string { return m.Meta.ID })
	keyIDs := setFromIDs(keys, func(k *hostkey.HostKey) string { return k.Meta.ID })
	rlIDs := setFromIDs(rls, func(r *ratelimit.RateLimit) string { return r.Meta.ID })
	polByID := make(map[string]*policy.Policy, len(pols))
	polIDSet := make(idSet, len(pols))
	for _, p := range pols {
		polByID[p.Meta.ID] = p
		polIDSet[p.Meta.ID] = struct{}{}
	}

	s.addProviders(provs)
	s.addRateLimits(rls)
	s.addHosts(hosts, polByID)
	s.addPolicies(pols, modelIDs, keyIDs, rlIDs)
	s.addModels(models, providerIDs)
	s.addHostKeys(keys, hostIDs, polByID)
	s.addRelayKeys(rks, polIDSet)
	s.computePolicyReverseJoins()
	s.addPricings(pricings, hostIDs, modelIDs)
	s.addBindings(bindings, modelIDs, hostIDs)
	// Aliases + policy allow-sets read bindings (BindingsForModel), so they
	// must run after bindings are indexed.
	for _, m := range s.modelsByID {
		s.indexModelSnapshots(m)
	}
	s.rebuildPolicyAllowSets()

	return s
}

func newEmptySnapshot(nProvs, nHosts, nPols, nRks, nModels, nKeys, nRLs, nPricings, nBindings int) *Snapshot {
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
		hostKeysByID:          make(map[string]*hostkey.HostKey, nKeys),
		rateLimitsByID:        make(map[string]*ratelimit.RateLimit, nRLs),
		rateLimitsByName:      make(map[string]*ratelimit.RateLimit, nRLs),
		relayKeysByID:         make(map[string]*relaykey.RelayKey, nRks),
		relayKeysByHash:       make(map[string]*relaykey.RelayKey, nRks),
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
	}
}
