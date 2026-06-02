// GET /{kind}/by-id/{id}/references — generic reverse-ref lookup.
//
// Returns every PG row that references the target entity. Used by the
// admin UI for blast-radius confirmation dialogs ("deleting this rate
// limit will affect N policies") and for inline "in use by" lists.
//
// Walks the relevant stores per kind; admin-only, no SLO. Indices come
// later if scan time matters. Reading PG (not the catalog snapshot) so
// disabled / soft-dropped refs are still visible — they exist in PG even
// when the data plane has filtered them out.
package control

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
)

type referenceItem struct {
	Kind string `json:"kind" doc:"Resource kind (host, policy, host-key, relay-key, model, pricing)."`
	ID   string `json:"id"   doc:"Resource id."`
	Name string `json:"name" doc:"Resource slug."`
	Via  string `json:"via"  doc:"Field path on the referencing row that points at the target."`
}

type referencesOutput struct {
	Body struct {
		Items []referenceItem `json:"items"`
	}
}

type referencesInput struct {
	ID string `path:"id" doc:"Target resource id."`
}

// registerReferences installs the per-kind references endpoints.
func registerReferences(api huma.API, d Deps, protect huma.Middlewares) {
	register := func(plural, singular string, scan func(ctx context.Context, id string) ([]referenceItem, error)) {
		huma.Register(api, huma.Operation{
			OperationID: "list_" + singular + "_references",
			Method:      http.MethodGet,
			Path:        "/" + plural + "/by-id/{id}/references",
			Summary:     "List rows that reference this " + singular,
			Tags:        []string{plural},
			Middlewares: protect,
			Errors:      []int{401, 500},
		}, func(ctx context.Context, in *referencesInput) (*referencesOutput, error) {
			if err := d.Authz.Authorize(ctx, plural+".read", authz.Resource{Kind: singular, ID: in.ID}); err != nil {
				return nil, mapAuthzErr(err)
			}
			items, err := scan(ctx, in.ID)
			if err != nil {
				return nil, huma.Error500InternalServerError(err.Error())
			}
			sortReferences(items)
			out := &referencesOutput{}
			out.Body.Items = items
			return out, nil
		})
	}

	register("providers", "provider", func(ctx context.Context, id string) ([]referenceItem, error) {
		return scanProviderRefs(ctx, d, id)
	})
	register("hosts", "host", func(ctx context.Context, id string) ([]referenceItem, error) {
		return scanHostRefs(ctx, d, id)
	})
	register("models", "model", func(ctx context.Context, id string) ([]referenceItem, error) {
		return scanModelRefs(ctx, d, id)
	})
	register("policies", "policy", func(ctx context.Context, id string) ([]referenceItem, error) {
		return scanPolicyRefs(ctx, d, id)
	})
	register("host-keys", "host-key", func(ctx context.Context, id string) ([]referenceItem, error) {
		return scanHostKeyRefs(ctx, d, id)
	})
	register("rate-limits", "rate-limit", func(ctx context.Context, id string) ([]referenceItem, error) {
		return scanRateLimitRefs(ctx, d, id)
	})
}

func sortReferences(items []referenceItem) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind
		}
		return items[i].Name < items[j].Name
	})
}

func scanProviderRefs(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
	out := []referenceItem{}
	models, err := d.Stores.Model.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	for _, m := range models {
		if m.Meta.Owner.ID == id {
			out = append(out, referenceItem{Kind: "model", ID: m.Meta.ID, Name: m.Meta.Name, Via: "metadata.owner.id"})
		}
	}
	return out, nil
}

func scanHostRefs(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
	out := []referenceItem{}
	keys, err := d.Stores.HostKey.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list host-keys: %w", err)
	}
	for _, k := range keys {
		if k.Spec.HostID == id {
			out = append(out, referenceItem{Kind: "host-key", ID: k.Meta.ID, Name: k.Meta.Name, Via: "spec.hostId"})
		}
	}
	bindings, err := d.Stores.Binding.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list bindings: %w", err)
	}
	for _, b := range bindings {
		if b.Spec.HostID == id {
			out = append(out, referenceItem{Kind: "host-binding", ID: b.Meta.ID, Name: b.Meta.Name, Via: "spec.hostId"})
		}
	}
	pricings, err := d.Stores.Pricing.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pricings: %w", err)
	}
	for _, p := range pricings {
		if p.Meta.Owner.ID == id {
			out = append(out, referenceItem{Kind: "pricing", ID: p.Meta.ID, Name: p.Meta.Name, Via: "metadata.owner.id"})
		}
	}
	return out, nil
}

func scanModelRefs(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
	out := []referenceItem{}
	pols, err := d.Stores.Policy.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list policies: %w", err)
	}
	for _, p := range pols {
		for _, mid := range p.Spec.ModelIDs {
			if mid == id {
				out = append(out, referenceItem{Kind: "policy", ID: p.Meta.ID, Name: p.Meta.Name, Via: "spec.modelIds"})
				break
			}
		}
	}
	pricings, err := d.Stores.Pricing.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pricings: %w", err)
	}
	for _, p := range pricings {
		for _, mid := range p.Spec.TargetModelIDs {
			if mid == id {
				out = append(out, referenceItem{Kind: "pricing", ID: p.Meta.ID, Name: p.Meta.Name, Via: "spec.targetModels"})
				break
			}
		}
	}
	return out, nil
}

func scanPolicyRefs(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
	out := []referenceItem{}
	rks, err := d.Stores.RelayKey.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list relay-keys: %w", err)
	}
	for _, k := range rks {
		if k.Spec.PolicyID == id {
			out = append(out, referenceItem{Kind: "relay-key", ID: k.Meta.ID, Name: k.Meta.Name, Via: "spec.policyId"})
		}
	}
	keys, err := d.Stores.HostKey.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list host-keys: %w", err)
	}
	for _, k := range keys {
		if k.Spec.PolicyID == id {
			out = append(out, referenceItem{Kind: "host-key", ID: k.Meta.ID, Name: k.Meta.Name, Via: "spec.policyId"})
		}
	}
	hosts, err := d.Stores.Host.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list hosts: %w", err)
	}
	for _, h := range hosts {
		matched := false
		for _, pid := range h.Spec.Policies {
			if pid == id {
				out = append(out, referenceItem{Kind: "host", ID: h.Meta.ID, Name: h.Meta.Name, Via: "spec.policies"})
				matched = true
				break
			}
		}
		if !matched && h.Spec.DefaultPolicy == id {
			out = append(out, referenceItem{Kind: "host", ID: h.Meta.ID, Name: h.Meta.Name, Via: "spec.defaultPolicy"})
		}
	}
	return out, nil
}

func scanHostKeyRefs(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
	out := []referenceItem{}
	pols, err := d.Stores.Policy.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list policies: %w", err)
	}
	for _, p := range pols {
		for _, kid := range p.Spec.HostKeyIDs {
			if kid == id {
				out = append(out, referenceItem{Kind: "policy", ID: p.Meta.ID, Name: p.Meta.Name, Via: "spec.hostKeyIds"})
				break
			}
		}
	}
	return out, nil
}

func scanRateLimitRefs(ctx context.Context, d Deps, id string) ([]referenceItem, error) {
	out := []referenceItem{}
	pols, err := d.Stores.Policy.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list policies: %w", err)
	}
	for _, p := range pols {
		if p.Spec.RateLimitID == id {
			out = append(out, referenceItem{Kind: "policy", ID: p.Meta.ID, Name: p.Meta.Name, Via: "spec.rateLimitId"})
			continue
		}
		for _, b := range p.Spec.RLBindings {
			if b.RateLimitID == id {
				out = append(out, referenceItem{Kind: "policy", ID: p.Meta.ID, Name: p.Meta.Name, Via: "spec.rlBindings[].rateLimitId"})
				break
			}
		}
	}
	return out, nil
}
