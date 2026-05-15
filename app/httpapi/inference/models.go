package inference

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/routing"
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
// the authenticated relay key (via its Policy.ModelIDs).
func registerModels(api huma.API, d Deps, mw huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "list_models",
		Method:      "GET",
		Path:        "/v1/models",
		Summary:     "List models accessible to the caller (OpenAI-compatible)",
		Tags:        []string{"inference"},
		Middlewares: mw,
		Errors:      []int{401, 500},
	}, func(ctx context.Context, _ *struct{}) (*modelsOutput, error) {
		rk := RelayKeyFromContext(ctx)
		if rk == nil {
			return nil, huma.Error401Unauthorized("missing relay key")
		}
		snap := d.Catalog.Current()
		pol, ok := snap.Policy(rk.Spec.PolicyID)
		if !ok {
			return nil, huma.Error500InternalServerError("policy not found for relay key")
		}

		out := &modelsOutput{}
		out.Body.Object = "list"
		// Enumerate every enabled model and ask routing whether the
		// policy reaches it. Covers all three grant forms (literal
		// ModelIDs, modelref Spec.Models, and implicit-wildcard when
		// both are empty) without duplicating the logic here.
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
