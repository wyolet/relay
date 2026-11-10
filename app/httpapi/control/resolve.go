package control

import (
	"context"
	"errors"
	"sort"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/pkg/modelref"
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
		Refs []string `json:"refs" doc:"The input references, in request order."`
		// Results is the per-ref expansion, in request order — each input
		// ref mapped to exactly what it covers. Consumers that classify or
		// count per individual ref (host-requirement grouping, diagnostics,
		// dead-ref detection) read this; the union fields below are the
		// flattened convenience view.
		Results []refResult `json:"results" doc:"Each input reference's own expansion, in request order."`
		// Unresolved lists the input refs that expanded to nothing (no
		// enabled binding matched) — dead refs.
		Unresolved []string            `json:"unresolved" doc:"Input references that match no enabled binding."`
		Models     []resolveEntity     `json:"models"     doc:"Every matched model, deduplicated across references."`
		Hosts      []resolveEntity     `json:"hosts"      doc:"Every matched host, deduplicated across references."`
		Bindings   []resolveBindingRef `json:"bindings"   doc:"Every matched (model, host) binding, deduplicated."`
		// Expanded is the canonical "provider/model@host" string for
		// every matched binding. Sorted, deduplicated. The UI uses this
		// as the authoritative "what does this picker grant" list.
		Expanded []string `json:"expanded" doc:"provider/model@host for every matched binding, sorted and deduplicated."`
	}
}

// refResult is one input ref's own expansion. Bindings within a ref are
// unique by construction; Expanded is sorted. Empty Expanded ⇒ dead ref.
type refResult struct {
	Ref      string              `json:"ref"      doc:"The input reference."`
	Expanded []string            `json:"expanded" doc:"provider/model@host for each binding it matches, sorted; empty for a dead reference."`
	ModelIDs []string            `json:"modelIds" doc:"Ids of the models it matches."`
	HostIDs  []string            `json:"hostIds"  doc:"Ids of the hosts it matches."`
	Bindings []resolveBindingRef `json:"bindings" doc:"The (model, host) bindings it matches."`
}

type resolveEntity struct {
	ID          string `json:"id"                    doc:"Row id."`
	Name        string `json:"name"                  doc:"Row slug."`
	DisplayName string `json:"displayName,omitempty" doc:"Display name."`
	Deprecated  string `json:"deprecated,omitempty"  doc:"The model's deprecation status, when not active."`
}

type resolveBindingRef struct {
	ModelID string `json:"modelId" doc:"Model id."`
	HostID  string `json:"hostId"  doc:"Host id."`
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
		Description: "Expands each model reference (provider, provider/model, provider/model@host, with wildcards) against what the data plane serves now: per reference, and as a deduplicated union. A malformed reference answers 400; one that matches nothing is listed in unresolved.",
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
