package control

import (
	"context"
	"errors"

	"github.com/danielgtaylor/huma/v2"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/modelref"
)

// resolveQuery is the GET /catalog/resolve query string input.
type resolveQuery struct {
	Ref string `query:"ref" required:"true" doc:"Catalog ref string in the modelref DSL: provider[/model[@host]]; * allowed in model and host positions."`
}

// resolveOutput is the body shape — see registerResolve doc for examples.
type resolveOutput struct {
	Body struct {
		Ref      string         `json:"ref"`
		Kind     string         `json:"kind"`
		Provider *resolveEntity `json:"provider,omitempty"`
		Models   []resolveEntity `json:"models"`
		Hosts    []resolveEntity `json:"hosts"`
		Bindings []resolveBindingRef `json:"bindings"`
	}
}

type resolveEntity struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	// Deprecated is "deprecated" | "sunset" | "" (active or unset). Set
	// only on model entries; provider/host entries leave it blank. The
	// picker uses this to grey out / badge wildcard matches that would
	// be excluded from a Policy without IncludeDeprecated.
	Deprecated string `json:"deprecated,omitempty"`
}

type resolveBindingRef struct {
	ModelID string `json:"modelId"`
	HostID  string `json:"hostId"`
}

// registerResolve installs GET /catalog/resolve — the picker-friendly
// expansion of a modelref string against the current snapshot. Returns
// the matched provider, models, hosts, and concrete (modelID, hostID)
// bindings the policy would grant. 400 on invalid syntax; 200 with
// empty sets on valid-but-no-match.
//
// Examples:
//
//	GET /catalog/resolve?ref=anthropic
//	→ kind=provider, provider={anthropic,...}, models=[claude-*],
//	  hosts=[anthropic, amazon-bedrock, ...], bindings=[all].
//
//	GET /catalog/resolve?ref=anthropic/claude-opus-4-7@bedrock
//	→ kind=binding, single model + single host + one binding.
func registerResolve(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "catalog_resolve",
		Method:      "GET",
		Path:        "/catalog/resolve",
		Summary:     "Resolve a modelref into matched catalog rows",
		Tags:        []string{"catalog"},
		Middlewares: protect,
		Errors:      []int{400, 401, 500},
	}, func(_ context.Context, in *resolveQuery) (*resolveOutput, error) {
		ref, err := modelref.Parse(in.Ref)
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

		snap := d.Catalog.Current()
		out := &resolveOutput{}
		out.Body.Ref = ref.Raw
		out.Body.Kind = string(ref.Kind())
		out.Body.Models = []resolveEntity{}
		out.Body.Hosts = []resolveEntity{}
		out.Body.Bindings = []resolveBindingRef{}

		// Host-only refs (@host): no provider to set, walk every model.
		// Provider-anchored refs: resolve provider first, then walk its
		// models. Unknown provider on a provider-anchored ref returns
		// an empty envelope (not 404) so the UI renders "nothing
		// matches" without branching.
		var modelsToWalk []*model.Model
		if ref.ProviderWildcard {
			modelsToWalk = snap.AllModels()
		} else {
			prov, ok := snap.ProviderByName(ref.Provider)
			if !ok {
				return out, nil
			}
			out.Body.Provider = &resolveEntity{
				ID:          prov.Meta.ID,
				Name:        prov.Meta.Name,
				DisplayName: prov.Meta.DisplayName,
			}
			modelsToWalk = snap.ModelsByProvider(prov.Meta.ID)
		}

		seenHost := map[string]struct{}{}
		for _, m := range modelsToWalk {
			if !m.IsEnabled() {
				continue
			}
			// Skip models that don't match the ref's model segment.
			if !ref.ModelWildcard && m.Meta.Name != ref.Model {
				continue
			}
			modelMatched := false
			for i := range m.Spec.Hosts {
				hb := &m.Spec.Hosts[i]
				if !hb.IsEnabled() {
					continue
				}
				h, ok := snap.Host(hb.HostID)
				if !ok {
					continue
				}
				providerSlug, _ := snap.ProviderSlug(m.Meta.Owner.ID)
				if !ref.Matches(providerSlug, m.Meta.Name, h.Meta.Name) {
					continue
				}
				modelMatched = true
				out.Body.Bindings = append(out.Body.Bindings, resolveBindingRef{
					ModelID: m.Meta.ID,
					HostID:  h.Meta.ID,
				})
				if _, dup := seenHost[h.Meta.ID]; !dup {
					seenHost[h.Meta.ID] = struct{}{}
					out.Body.Hosts = append(out.Body.Hosts, entityFromHost(h))
				}
			}
			if modelMatched {
				out.Body.Models = append(out.Body.Models, entityFromModel(m))
			}
		}
		return out, nil
	})
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

// Compile-time guard that Deps carries the snapshot accessor we need.
var _ = func(d Deps) *appcatalog.Snapshot { return d.Catalog.Current() }
