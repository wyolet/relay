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
	Refs []string `query:"ref,explode" required:"true" doc:"One or more catalog refs in the modelref DSL: provider[/model][@host], or @host. Repeat the parameter to union multiple refs."`
}

// resolveOutput is the union expansion of all refs in the request.
type resolveOutput struct {
	Body struct {
		Refs     []string            `json:"refs"`
		Models   []resolveEntity     `json:"models"`
		Hosts    []resolveEntity     `json:"hosts"`
		Bindings []resolveBindingRef `json:"bindings"`
		// Expanded is the canonical "provider/model@host" string for
		// every matched binding. Sorted, deduplicated. The UI uses this
		// as the authoritative "what does this picker grant" list.
		Expanded []string `json:"expanded"`
	}
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
// one or more modelref strings against Postgres (not the data-plane
// snapshot) and returns the union of matched models, hosts, bindings,
// and the canonical "provider/model@host" string for every binding.
//
// Reading PG keeps the control plane independent of snapshot health
// and includes disabled rows the operator may legitimately want to see
// when authoring a policy. The data-plane sanitizer is the gate for
// what traffic can actually use.
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

		idx, err := loadResolveIndex(ctx, d)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}

		out := &resolveOutput{}
		out.Body.Refs = in.Refs
		out.Body.Models = []resolveEntity{}
		out.Body.Hosts = []resolveEntity{}
		out.Body.Bindings = []resolveBindingRef{}
		out.Body.Expanded = []string{}

		seenModel := map[string]struct{}{}
		seenHost := map[string]struct{}{}
		seenBinding := map[string]struct{}{}

		for _, ref := range parsed {
			expandRef(idx, ref, out, seenModel, seenHost, seenBinding)
		}

		sort.Strings(out.Body.Expanded)
		return out, nil
	})
}

// resolveIndex is a small ad-hoc graph built from store.List() per
// request — the control-plane equivalent of what the data-plane
// snapshot provides. Cheap rebuild; admin endpoint, no SLO.
type resolveIndex struct {
	providersByID    map[string]*provider.Provider
	providersByName  map[string]*provider.Provider
	hostsByID        map[string]*host.Host
	allModels        []*model.Model
	modelsByProvider map[string][]*model.Model
}

func loadResolveIndex(ctx context.Context, d Deps) (*resolveIndex, error) {
	provs, err := d.Stores.Provider.List(ctx)
	if err != nil {
		return nil, err
	}
	hosts, err := d.Stores.Host.List(ctx)
	if err != nil {
		return nil, err
	}
	models, err := d.Stores.Model.List(ctx)
	if err != nil {
		return nil, err
	}
	idx := &resolveIndex{
		providersByID:    make(map[string]*provider.Provider, len(provs)),
		providersByName:  make(map[string]*provider.Provider, len(provs)),
		hostsByID:        make(map[string]*host.Host, len(hosts)),
		allModels:        models,
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
		idx.modelsByProvider[m.Meta.Owner.ID] = append(idx.modelsByProvider[m.Meta.Owner.ID], m)
	}
	for k := range idx.modelsByProvider {
		sort.Slice(idx.modelsByProvider[k], func(i, j int) bool {
			return idx.modelsByProvider[k][i].Meta.Name < idx.modelsByProvider[k][j].Meta.Name
		})
	}
	sort.Slice(idx.allModels, func(i, j int) bool {
		return idx.allModels[i].Meta.Name < idx.allModels[j].Meta.Name
	})
	return idx, nil
}

// expandRef walks the index for one ref and appends matches to out,
// skipping anything already seen via the dedup maps.
func expandRef(
	idx *resolveIndex,
	ref modelref.Ref,
	out *resolveOutput,
	seenModel, seenHost, seenBinding map[string]struct{},
) {
	var modelsToWalk []*model.Model
	if ref.ProviderWildcard {
		modelsToWalk = idx.allModels
	} else {
		prov, ok := idx.providersByName[ref.Provider]
		if !ok {
			return
		}
		modelsToWalk = idx.modelsByProvider[prov.Meta.ID]
	}

	for _, m := range modelsToWalk {
		if !ref.ModelWildcard && m.Meta.Name != ref.Model {
			continue
		}
		var providerSlug string
		if p, ok := idx.providersByID[m.Meta.Owner.ID]; ok {
			providerSlug = p.Meta.Name
		}
		modelMatched := false
		for i := range m.Spec.Hosts {
			hb := &m.Spec.Hosts[i]
			h, ok := idx.hostsByID[hb.HostID]
			if !ok {
				continue
			}
			if !ref.Matches(providerSlug, m.Meta.Name, h.Meta.Name) {
				continue
			}
			modelMatched = true

			bindKey := m.Meta.ID + "|" + h.Meta.ID
			if _, dup := seenBinding[bindKey]; !dup {
				seenBinding[bindKey] = struct{}{}
				out.Body.Bindings = append(out.Body.Bindings, resolveBindingRef{
					ModelID: m.Meta.ID,
					HostID:  h.Meta.ID,
				})
				out.Body.Expanded = append(out.Body.Expanded,
					providerSlug+"/"+m.Meta.Name+"@"+h.Meta.Name)
			}
			if _, dup := seenHost[h.Meta.ID]; !dup {
				seenHost[h.Meta.ID] = struct{}{}
				out.Body.Hosts = append(out.Body.Hosts, entityFromHost(h))
			}
		}
		if modelMatched {
			if _, dup := seenModel[m.Meta.ID]; !dup {
				seenModel[m.Meta.ID] = struct{}{}
				out.Body.Models = append(out.Body.Models, entityFromModel(m))
			}
		}
	}
}

func entityFromModel(m *model.Model) resolveEntity {
	e := resolveEntity{ID: m.Meta.ID, Name: m.Meta.Name, DisplayName: m.Meta.DisplayName}
	if m.Spec.Deprecation != nil {
		e.Deprecated = string(m.Spec.Deprecation.Status)
	}
	return e
}

func entityFromHost(h *host.Host) resolveEntity {
	return resolveEntity{ID: h.Meta.ID, Name: h.Meta.Name, DisplayName: h.Meta.DisplayName}
}
