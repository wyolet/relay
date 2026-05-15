package inference

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/app/settings"
)

// modelObject is the OpenAI list-models entry shape.
type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

type modelsOutput struct {
	Body struct {
		Object string        `json:"object"`
		Data   []modelObject `json:"data"`
	}
}

// registerModels serves GET /v1/models — list of models accessible to
// the authenticated relay key.
//
// Policy-bound key: enumerates every enabled model and asks
// routing.PolicyAllows. Covers literal ModelIDs grants, modelref
// Spec.Models grants, and the implicit-wildcard case (both fields
// empty).
//
// Policy-less key (Spec.PolicyID empty + settings.Inference.
// AllowMissingPolicy on): returns every enabled model that has at
// least one enabled host binding to a host the relay has hostkeys for.
func registerModels(api huma.API, d Deps, mw huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "list_models",
		Method:      "GET",
		Path:        "/v1/models",
		Summary:     "List models accessible to the caller (OpenAI-compatible)",
		Tags:        []string{"inference"},
		Middlewares: mw,
		Errors:      []int{401, 403, 500},
	}, func(ctx context.Context, _ *struct{}) (*modelsOutput, error) {
		rk := RelayKeyFromContext(ctx)
		if rk == nil {
			return nil, huma.Error401Unauthorized("missing relay key")
		}
		snap := d.Catalog.Current()
		out := &modelsOutput{}
		out.Body.Object = "list"

		if rk.Spec.PolicyID == "" {
			v, _ := d.Catalog.Setting(settings.SectionInference)
			cfg, _ := v.(*settings.Inference)
			if cfg == nil || !cfg.AllowMissingPolicy {
				return nil, huma.Error403Forbidden("policy-less traffic is disabled on this relay")
			}
			seen := map[string]struct{}{}
			for _, m := range snap.AllModels() {
				if _, dup := seen[m.Meta.Name]; dup {
					continue
				}
				for i := range m.Spec.Hosts {
					hb := &m.Spec.Hosts[i]
					if !hb.IsEnabled() {
						continue
					}
					if len(snap.HostKeysForHost(hb.HostID)) == 0 {
						continue
					}
					ownedBy := ""
					if pname, ok := snap.ProviderSlug(m.Meta.Owner.ID); ok {
						ownedBy = pname
					}
					out.Body.Data = append(out.Body.Data, modelObject{
						ID:      m.Meta.Name,
						Object:  "model",
						OwnedBy: ownedBy,
					})
					seen[m.Meta.Name] = struct{}{}
					break
				}
			}
			return out, nil
		}

		pol, ok := snap.Policy(rk.Spec.PolicyID)
		if !ok {
			return nil, huma.Error500InternalServerError("policy not found for relay key")
		}
		seen := map[string]struct{}{}
		for _, m := range snap.AllModels() {
			if _, dup := seen[m.Meta.Name]; dup {
				continue
			}
			if !routing.PolicyAllows(snap, pol, m) {
				continue
			}
			seen[m.Meta.Name] = struct{}{}
			ownedBy := ""
			if pname, ok := snap.ProviderSlug(m.Meta.Owner.ID); ok {
				ownedBy = pname
			}
			out.Body.Data = append(out.Body.Data, modelObject{
				ID:      m.Meta.Name,
				Object:  "model",
				OwnedBy: ownedBy,
			})
		}
		return out, nil
	})
}
