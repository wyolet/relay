package control

import (
	"context"
	"errors"
	"sort"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/modelref"
	"github.com/wyolet/relay/app/provider"
)

// resolveQuery accepts one or more refs. Pass each as a repeated query
// parameter: GET /catalog/resolve?ref=anthropic&ref=openai/gpt-5
type resolveQuery struct {
	Refs              []string `query:"ref,explode" required:"true" doc:"One or more catalog refs in the modelref DSL: provider[/model][@host], or @host. Repeat the parameter to union multiple refs."`
	IncludeDeprecated bool     `query:"includeDeprecated" doc:"Include deprecated models in the expansion. Default false drops them, matching /catalog/graph so counts agree with the picker."`
}

// resolveOutput carries both the per-ref breakdown (Results/Unresolved) and
// the deduplicated union across all refs (Models/Hosts/Bindings/Expanded).
type resolveOutput struct {
	Body struct {
		Refs []string `json:"refs"`
		// Results is the per-ref expansion, in request order — each input
		// ref mapped to exactly what it covers. Consumers that classify or
		// count per individual ref (host-requirement grouping, diagnostics,
		// dead-ref detection) read this; the union fields below are the
		// flattened convenience view.
		Results []refResult `json:"results"`
		// Unresolved lists the input refs that expanded to nothing (no
		// enabled binding matched) — dead refs.
		Unresolved []string            `json:"unresolved"`
		Models     []resolveEntity     `json:"models"`
		Hosts      []resolveEntity     `json:"hosts"`
		Bindings   []resolveBindingRef `json:"bindings"`
		// Expanded is the canonical "provider/model@host" string for
		// every matched binding. Sorted, deduplicated. The UI uses this
		// as the authoritative "what does this picker grant" list.
		Expanded []string `json:"expanded"`
	}
}

// refResult is one input ref's own expansion. Bindings within a ref are
// unique by construction; Expanded is sorted. Empty Expanded ⇒ dead ref.
type refResult struct {
	Ref      string              `json:"ref"`
	Expanded []string            `json:"expanded"`
	ModelIDs []string            `json:"modelIds"`
	HostIDs  []string            `json:"hostIds"`
	Bindings []resolveBindingRef `json:"bindings"`
}

type resolveEntity struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	Deprecated  string `json:"deprecated,omitempty"`
}

type resolveBindingRef struct {
	ModelID string `json:"modelId"`
	HostID  string `json:"hostId"`
}

// registerResolve installs GET /catalog/resolve. Each request resolves
// one or more modelref strings against the in-memory snapshot and returns
// both a per-ref breakdown (results[] + unresolved[]) and the deduplicated
// union (models/hosts/bindings/expanded).
//
// Snapshot-backed (same source as /catalog/graph), so it reflects exactly
// what the data plane can route to now: disabled providers/hosts/models are
// absent, disabled bindings are skipped, and deprecated models are dropped
// unless includeDeprecated=true (mirroring graph, so counts agree with the
// picker). Full-list / detail views use the CRUD APIs (PG) for disabled rows.
//
// 400 on any malformed ref; 200 with empty sets on valid-but-no-match.
func registerResolve(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "catalog_resolve",
		Method:      "GET",
		Path:        "/catalog/resolve",
		Summary:     "Resolve one or more modelrefs into matched catalog rows",
		Tags:        []string{"catalog"},
		Middlewares: protect,
		Errors:      []int{400, 401, 500},
	}, func(ctx context.Context, in *resolveQuery) (*resolveOutput, error) {
		if len(in.Refs) == 0 {
			return nil, huma.Error400BadRequest("at least one ref is required")
		}

		parsed := make([]modelref.Ref, 0, len(in.Refs))
		for _, raw := range in.Refs {
			r, err := modelref.Parse(raw)
			if err != nil {
				var se *modelref.SyntaxError
				if errors.As(err, &se) {
					return nil, huma.Error400BadRequest(se.Error())
				}
				if errors.Is(err, modelref.ErrEmpty) {
					return nil, huma.Error400BadRequest("ref is required")
				}
				return nil, huma.Error400BadRequest(err.Error())
			}
			parsed = append(parsed, r)
		}

		idx, err := loadResolveIndex(d)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}

		out := &resolveOutput{}
		out.Body.Refs = in.Refs
		out.Body.Results = make([]refResult, 0, len(parsed))
		out.Body.Unresolved = []string{}
		out.Body.Models = []resolveEntity{}
		out.Body.Hosts = []resolveEntity{}
		out.Body.Bindings = []resolveBindingRef{}
		out.Body.Expanded = []string{}

		seenModel := map[string]struct{}{}
		seenHost := map[string]struct{}{}
		seenBinding := map[string]struct{}{}
		seenExpanded := map[string]struct{}{}

		for i, ref := range parsed {
			r := expandOne(idx, in.Refs[i], ref, in.IncludeDeprecated)
			out.Body.Results = append(out.Body.Results, r)
			if len(r.Expanded) == 0 {
				out.Body.Unresolved = append(out.Body.Unresolved, in.Refs[i])
			}
			for _, b := range r.Bindings {
				key := b.ModelID + "|" + b.HostID
				if _, dup := seenBinding[key]; dup {
					continue
				}
				seenBinding[key] = struct{}{}
				out.Body.Bindings = append(out.Body.Bindings, b)
			}
			for _, e := range r.Expanded {
				if _, dup := seenExpanded[e]; dup {
					continue
				}
				seenExpanded[e] = struct{}{}
				out.Body.Expanded = append(out.Body.Expanded, e)
			}
			for _, mid := range r.ModelIDs {
				if _, dup := seenModel[mid]; dup {
					continue
				}
				seenModel[mid] = struct{}{}
				if m, ok := idx.modelsByID[mid]; ok {
					out.Body.Models = append(out.Body.Models, entityFromModel(m))
				}
			}
			for _, hid := range r.HostIDs {
				if _, dup := seenHost[hid]; dup {
					continue
				}
				seenHost[hid] = struct{}{}
				if h, ok := idx.hostsByID[hid]; ok {
					out.Body.Hosts = append(out.Body.Hosts, entityFromHost(h))
				}
			}
		}

		sort.Strings(out.Body.Expanded)
		return out, nil
	})
}

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
		for i := range m.Spec.Hosts {
			hb := &m.Spec.Hosts[i]
			if !hb.IsEnabled() {
				continue
			}
			h, ok := idx.hostsByID[hb.HostID]
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
