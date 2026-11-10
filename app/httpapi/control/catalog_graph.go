// GET /catalog/graph — the whole catalog as a minimal graph for the admin
// model picker.
//
// Returns providers, hosts, and models (with their host bindings) in one
// call so the UI can render and cross-link them without three separate
// list fetches + client-side flattening. Only the fields the picker and the
// policy model view actually read are included — identity (id/name/
// displayName), the enabled flag, host icon path, the featured flag (from
// the catalog's `labels.featured`), per-model deprecation,
// capabilities, context window, the pointer (the upstream wire name the
// bare model name resolves to at request time), and the (hostId, adapter,
// enabled) bindings. Heavy spec (full snapshot list, pricing refs, modalities, …)
// is deliberately omitted.
//
// Reads the in-memory snapshot via the same index as /catalog/resolve, so
// it returns exactly what the data plane can route to right now: disabled
// providers/hosts/models are already absent, and disabled bindings are
// pruned here. The picker only offers routable targets; full-list / detail
// views use the CRUD APIs (PG), which show disabled rows.

package control

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
)

type graphProvider struct {
	ID          string `json:"id"                    doc:"Provider id."`
	Name        string `json:"name"                  doc:"Provider slug."`
	DisplayName string `json:"displayName,omitempty" doc:"Display name."`
}

type graphHost struct {
	ID          string `json:"id"                    doc:"Host id."`
	Name        string `json:"name"                  doc:"Host slug."`
	DisplayName string `json:"displayName,omitempty" doc:"Display name."`
	IconPath    string `json:"iconPath,omitempty"    doc:"Icon asset path."`
}

type graphBinding struct {
	HostID  string `json:"hostId"  doc:"Id of a host serving the model."`
	Adapter string `json:"adapter" doc:"Wire shape relay speaks to that host for the model."`
}

type graphModel struct {
	ID                 string             `json:"id"                           doc:"Model id."`
	Name               string             `json:"name"                         doc:"Model slug."`
	DisplayName        string             `json:"displayName,omitempty"        doc:"Display name."`
	ProviderID         string             `json:"providerId"                   doc:"Id of the model's provider."`
	Featured           bool               `json:"featured,omitempty"           doc:"The catalog marks the model as featured."`
	Deprecated         string             `json:"deprecated,omitempty"         doc:"Deprecation status, when not active."`
	Capabilities       model.Capabilities `json:"capabilities,omitempty"       doc:"What the model supports."`
	ContextWindowTotal int                `json:"contextWindowTotal,omitempty" doc:"Context window in tokens."`
	ContextWindowInput int                `json:"contextWindowInput,omitempty" doc:"Published input cap in tokens."`
	Pointer            string             `json:"pointer,omitempty"            doc:"Snapshot the bare model name resolves to."`
	Bindings           []graphBinding     `json:"bindings"                     doc:"Enabled hosts serving the model."`
}

type graphOutput struct {
	Body struct {
		Providers []graphProvider `json:"providers" doc:"Providers in the catalog."`
		Hosts     []graphHost     `json:"hosts"     doc:"Hosts in the catalog."`
		Models    []graphModel    `json:"models"    doc:"Models, each with the hosts that serve it."`
	}
}

type graphQuery struct {
	IncludeDeprecated bool     `query:"includeDeprecated" doc:"Include deprecated models (still flagged via 'deprecated'). Default false drops them server-side."`
	Label             []string `query:"label,explode" doc:"Label selector key=value (repeatable, all must match). e.g. label=featured=true."`
}

// registerCatalogGraph installs GET /catalog/graph.
func registerCatalogGraph(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "catalog_graph",
		Method:      "GET",
		Path:        "/catalog/graph",
		Summary:     "Minimal catalog graph (providers, hosts, models + bindings) for the picker",
		Description: "Providers, hosts and models with their enabled bindings in one call, read from what the data plane serves now: disabled rows are absent. Deprecated models are left out unless includeDeprecated is set. The CRUD lists show every row.",
		Tags:        []string{"catalog"},
		Middlewares: protect,
		Errors:      []int{400, 401, 500},
	}, func(ctx context.Context, in *graphQuery) (*graphOutput, error) {
		sel, err := parseLabelSelector(in.Label)
		if err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}

		idx, err := loadResolveIndex(d)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}

		out := &graphOutput{}
		out.Body.Providers = graphProviders(idx)
		out.Body.Hosts = graphHosts(idx)
		out.Body.Models = graphModels(idx, in.IncludeDeprecated, sel)
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

func graphModels(idx *resolveIndex, includeDeprecated bool, sel map[string]string) []graphModel {
	out := make([]graphModel, 0, len(idx.allModels))
	for _, m := range idx.allModels {
		dep := deprecationStatus(m)
		if !includeDeprecated && dep != "" {
			continue
		}
		if !matchesLabels(m.Meta.Labels, sel) {
			continue
		}
		gm := graphModel{
			ID:                 m.Meta.ID,
			Name:               m.Meta.Name,
			DisplayName:        m.Meta.DisplayName,
			ProviderID:         m.Meta.Owner.ID,
			Featured:           m.Meta.Labels["featured"] == "true",
			Deprecated:         dep,
			Capabilities:       m.Spec.Capabilities,
			ContextWindowTotal: m.Spec.ContextWindowTotal,
			ContextWindowInput: m.Spec.ContextWindowInput,
			Pointer:            m.PointerUpstream(),
			Bindings:           []graphBinding{},
		}
		for _, hb := range idx.snap.BindingsForModel(m.Meta.ID) {
			if !hb.IsEnabled() {
				continue
			}
			gm.Bindings = append(gm.Bindings, graphBinding{
				HostID:  hb.Spec.HostID,
				Adapter: string(hb.Spec.Adapter),
			})
		}
		out = append(out, gm)
	}
	return out
}

// parseLabelSelector turns repeated key=value query params into an AND-set.
// Mirrors pkg/filter's ?label semantics (same 400 message) so the graph and
// the CRUD list endpoints validate selectors identically.
func parseLabelSelector(raw []string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	sel := make(map[string]string, len(raw))
	for _, kv := range raw {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("label: must be key=value")
		}
		sel[k] = v
	}
	return sel, nil
}

// matchesLabels reports whether labels satisfies every key=value in sel.
// Empty sel matches everything.
func matchesLabels(labels, sel map[string]string) bool {
	for k, v := range sel {
		if labels[k] != v {
			return false
		}
	}
	return true
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
