package control

import (
	"errors"
	"sort"

	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/modelref"
	"github.com/wyolet/relay/app/provider"
)

// resolveIndex is a small ad-hoc graph built from the in-memory catalog
// snapshot — the same source the data plane routes against. Reading the
// snapshot (not PG) is deliberate: resolve/graph answer "what can we route
// to right now", so they must reflect exactly the enabled, reconciled set
// the data plane sees. Disabled providers/hosts/models are already absent
// from the snapshot; binding-level Enabled is the one dimension callers
// must still honour (the snapshot keeps disabled bindings on enabled
// models). Full-list / detail views use the CRUD APIs (PG) instead — they
// intentionally show disabled rows.
type resolveIndex struct {
	snap             *catalog.Snapshot
	providersByID    map[string]*provider.Provider
	providersByName  map[string]*provider.Provider
	hostsByID        map[string]*host.Host
	allModels        []*model.Model
	modelsByID       map[string]*model.Model
	modelsByProvider map[string][]*model.Model
}

func loadResolveIndex(d Deps) (*resolveIndex, error) {
	if d.Catalog == nil {
		return nil, errors.New("catalog not ready")
	}
	snap := d.Catalog.Current()
	if snap == nil {
		return nil, errors.New("catalog snapshot not ready")
	}
	provs := snap.AllProviders()
	hosts := snap.Hosts()
	models := snap.AllModels() // already sorted by slug
	idx := &resolveIndex{
		snap:             snap,
		providersByID:    make(map[string]*provider.Provider, len(provs)),
		providersByName:  make(map[string]*provider.Provider, len(provs)),
		hostsByID:        make(map[string]*host.Host, len(hosts)),
		allModels:        models,
		modelsByID:       make(map[string]*model.Model, len(models)),
		modelsByProvider: map[string][]*model.Model{},
	}
	for _, p := range provs {
		idx.providersByID[p.Meta.ID] = p
		idx.providersByName[p.Meta.Name] = p
	}
	for _, h := range hosts {
		idx.hostsByID[h.Meta.ID] = h
	}
	for _, m := range models {
		idx.modelsByID[m.Meta.ID] = m
		idx.modelsByProvider[m.Meta.Owner.ID] = append(idx.modelsByProvider[m.Meta.Owner.ID], m)
	}
	return idx, nil
}

// expandOne walks the index for a single ref and returns its own expansion:
// the matched binding strings, plus the (deduplicated) model and host ids it
// covers. Disabled bindings are skipped; deprecated models are skipped unless
// includeDeprecated. Bindings are unique by construction, so no dedup needed.
func expandOne(idx *resolveIndex, raw string, ref modelref.Ref, includeDeprecated bool) refResult {
	res := refResult{
		Ref:      raw,
		Expanded: []string{},
		ModelIDs: []string{},
		HostIDs:  []string{},
		Bindings: []resolveBindingRef{},
	}

	var modelsToWalk []*model.Model
	if ref.ProviderWildcard {
		modelsToWalk = idx.allModels
	} else {
		prov, ok := idx.providersByName[ref.Provider]
		if !ok {
			return res
		}
		modelsToWalk = idx.modelsByProvider[prov.Meta.ID]
	}

	seenHost := map[string]struct{}{}
	for _, m := range modelsToWalk {
		if !ref.ModelWildcard && m.Meta.Name != ref.Model {
			continue
		}
		if !includeDeprecated && deprecationStatus(m) != "" {
			continue
		}
		var providerSlug string
		if p, ok := idx.providersByID[m.Meta.Owner.ID]; ok {
			providerSlug = p.Meta.Name
		}
		modelMatched := false
		for _, hb := range idx.snap.BindingsForModel(m.Meta.ID) {
			if !hb.IsEnabled() {
				continue
			}
			h, ok := idx.hostsByID[hb.Spec.HostID]
			if !ok {
				continue
			}
			if !ref.Matches(providerSlug, m.Meta.Name, h.Meta.Name) {
				continue
			}
			modelMatched = true
			res.Bindings = append(res.Bindings, resolveBindingRef{ModelID: m.Meta.ID, HostID: h.Meta.ID})
			res.Expanded = append(res.Expanded, providerSlug+"/"+m.Meta.Name+"@"+h.Meta.Name)
			if _, dup := seenHost[h.Meta.ID]; !dup {
				seenHost[h.Meta.ID] = struct{}{}
				res.HostIDs = append(res.HostIDs, h.Meta.ID)
			}
		}
		if modelMatched {
			res.ModelIDs = append(res.ModelIDs, m.Meta.ID)
		}
	}
	sort.Strings(res.Expanded)
	return res
}

func entityFromModel(m *model.Model) resolveEntity {
	return resolveEntity{
		ID:          m.Meta.ID,
		Name:        m.Meta.Name,
		DisplayName: m.Meta.DisplayName,
		Deprecated:  deprecationStatus(m),
	}
}

func entityFromHost(h *host.Host) resolveEntity {
	return resolveEntity{ID: h.Meta.ID, Name: h.Meta.Name, DisplayName: h.Meta.DisplayName}
}
