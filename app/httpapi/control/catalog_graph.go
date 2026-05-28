// GET /catalog/graph — the whole catalog as a minimal graph for the admin
// model picker.
//
// Returns providers, hosts, and models (with their host bindings) in one
// call so the UI can render and cross-link them without three separate
// list fetches + client-side flattening. Only the fields the picker and the
// policy model view actually read are included — identity (id/name/
// displayName), the enabled flag, host icon path, per-model deprecation,
// capabilities, context window, and the (hostId, adapter, enabled) bindings.
// Heavy spec (snapshots, pricing refs, modalities, …) is deliberately
// omitted.
//
// Reads the in-memory snapshot via the same index as /catalog/resolve, so
// it returns exactly what the data plane can route to right now: disabled
// providers/hosts/models are already absent, and disabled bindings are
// pruned here. The picker only offers routable targets; full-list / detail
// views use the CRUD APIs (PG), which show disabled rows.
package control

import (
	"context"
	"sort"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/provider"
)

type graphProvider struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
}

type graphHost struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	IconPath    string `json:"iconPath,omitempty"`
}

type graphBinding struct {
	HostID  string `json:"hostId"`
	Adapter string `json:"adapter"`
}

type graphModel struct {
	ID                 string             `json:"id"`
	Name               string             `json:"name"`
	DisplayName        string             `json:"displayName,omitempty"`
	ProviderID         string             `json:"providerId"`
	Deprecated         string             `json:"deprecated,omitempty"`
	Capabilities       model.Capabilities `json:"capabilities,omitempty"`
	ContextWindowTotal int                `json:"contextWindowTotal,omitempty"`
	ContextWindowInput int                `json:"contextWindowInput,omitempty"`
	Bindings           []graphBinding     `json:"bindings"`
}

type graphOutput struct {
	Body struct {
		Providers []graphProvider `json:"providers"`
		Hosts     []graphHost     `json:"hosts"`
		Models    []graphModel    `json:"models"`
	}
}

type graphQuery struct {
	IncludeDeprecated bool `query:"includeDeprecated" doc:"Include deprecated models (still flagged via 'deprecated'). Default false drops them server-side."`
}

// registerCatalogGraph installs GET /catalog/graph.
func registerCatalogGraph(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "catalog_graph",
		Method:      "GET",
		Path:        "/catalog/graph",
		Summary:     "Minimal catalog graph (providers, hosts, models + bindings) for the picker",
		Tags:        []string{"catalog"},
		Middlewares: protect,
		Errors:      []int{401, 500},
	}, func(ctx context.Context, in *graphQuery) (*graphOutput, error) {
		idx, err := loadResolveIndex(d)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}

		out := &graphOutput{}
		out.Body.Providers = graphProviders(idx)
		out.Body.Hosts = graphHosts(idx)
		out.Body.Models = graphModels(idx, in.IncludeDeprecated)
		return out, nil
	})
}

func graphProviders(idx *resolveIndex) []graphProvider {
	provs := make([]*provider.Provider, 0, len(idx.providersByID))
	for _, p := range idx.providersByID {
		provs = append(provs, p)
	}
	sort.Slice(provs, func(i, j int) bool { return provs[i].Meta.Name < provs[j].Meta.Name })
	out := make([]graphProvider, 0, len(provs))
	for _, p := range provs {
		out = append(out, graphProvider{ID: p.Meta.ID, Name: p.Meta.Name, DisplayName: p.Meta.DisplayName})
	}
	return out
}

func graphHosts(idx *resolveIndex) []graphHost {
	hosts := make([]*host.Host, 0, len(idx.hostsByID))
	for _, h := range idx.hostsByID {
		hosts = append(hosts, h)
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Meta.Name < hosts[j].Meta.Name })
	out := make([]graphHost, 0, len(hosts))
	for _, h := range hosts {
		gh := graphHost{
			ID:          h.Meta.ID,
			Name:        h.Meta.Name,
			DisplayName: h.Meta.DisplayName,
		}
		if h.Spec.Icon != nil {
			gh.IconPath = h.Spec.Icon.Path
		}
		out = append(out, gh)
	}
	return out
}

func graphModels(idx *resolveIndex, includeDeprecated bool) []graphModel {
	out := make([]graphModel, 0, len(idx.allModels))
	for _, m := range idx.allModels {
		dep := deprecationStatus(m)
		if !includeDeprecated && dep != "" {
			continue
		}
		gm := graphModel{
			ID:                 m.Meta.ID,
			Name:               m.Meta.Name,
			DisplayName:        m.Meta.DisplayName,
			ProviderID:         m.Meta.Owner.ID,
			Deprecated:         dep,
			Capabilities:       m.Spec.Capabilities,
			ContextWindowTotal: m.Spec.ContextWindowTotal,
			ContextWindowInput: m.Spec.ContextWindowInput,
			Bindings:           make([]graphBinding, 0, len(m.Spec.Hosts)),
		}
		for i := range m.Spec.Hosts {
			hb := &m.Spec.Hosts[i]
			if !hb.IsEnabled() {
				continue
			}
			gm.Bindings = append(gm.Bindings, graphBinding{
				HostID:  hb.HostID,
				Adapter: string(hb.Adapter),
			})
		}
		out = append(out, gm)
	}
	return out
}

// deprecationStatus returns a non-empty marker when the model is deprecated:
// the explicit status if present, else "deprecated" when only a date is set.
func deprecationStatus(m *model.Model) string {
	if m.Spec.Deprecation != nil && m.Spec.Deprecation.Status != "" {
		return string(m.Spec.Deprecation.Status)
	}
	if m.Spec.DeprecationDate != "" {
		return "deprecated"
	}
	return ""
}
