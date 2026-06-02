// subresources.go adds resource-navigation read endpoints — "API UX":
//
//	GET /models/{ref}/hosts     hosts serving this model (+ binding + pricing)
//	GET /models/{ref}/pricing   pricing per host for this model
//	GET /hosts/{ref}/models      models this host serves (+ binding + pricing)
//
// They answer "to see a model's hosts, GET the model's hosts" so consumers
// (the admin UI's model-detail tabs, future TUIs) navigate by resource
// instead of listing /host-bindings and joining client-side. Read-only, served
// from the in-memory snapshot (same source the data plane routes against, so
// they reflect the enabled+reconciled set — disabled rows are absent). Rates
// are embedded inline so a detail page renders pricing with no follow-up call.
//
// {ref} is a slug or UUID id (id wins). Pricing for a (model, host) is the
// binding's explicit PricingID, falling back to the pricing that owns the
// (model, host) pair.
package control

import (
	"context"
	"sort"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/pricing"
)

// ── shared row shapes ───────────────────────────────────────────────────────

type subHost struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	BaseURL     string `json:"baseURL,omitempty"`
}

type subModel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
}

type subBinding struct {
	ID           string   `json:"id"`
	Adapter      string   `json:"adapter"`
	UpstreamName string   `json:"upstreamName,omitempty"`
	Enabled      bool     `json:"enabled"`
	Snapshots    []string `json:"snapshots,omitempty"`
}

type subRate struct {
	Meter       string  `json:"meter"`
	Unit        string  `json:"unit"`
	Amount      float64 `json:"amount"`
	AboveTokens int     `json:"aboveTokens,omitempty"`
}

type subPricing struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Currency string    `json:"currency"`
	Rates    []subRate `json:"rates"`
}

func registerSubresources(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "model_hosts",
		Method:      "GET",
		Path:        "/models/{ref}/hosts",
		Summary:     "List the hosts that serve this model, with binding + pricing",
		Tags:        []string{"models"},
		Middlewares: protect,
		Errors:      []int{401, 404},
	}, func(ctx context.Context, in *refInput) (*modelHostsOutput, error) {
		snap := d.Catalog.Current()
		m, ok := modelByRef(snap, in.Ref)
		if !ok {
			return nil, huma.Error404NotFound("model not found")
		}
		out := &modelHostsOutput{}
		out.Body.Model = subModel{ID: m.Meta.ID, Name: m.Meta.Name, DisplayName: m.Meta.DisplayName}
		out.Body.Hosts = []modelHostRow{}
		for _, b := range snap.BindingsForModel(m.Meta.ID) {
			h, ok := snap.Host(b.Spec.HostID)
			if !ok {
				continue
			}
			out.Body.Hosts = append(out.Body.Hosts, modelHostRow{
				Host:    hostEntryOf(h),
				Binding: bindingEntryOf(b),
				Pricing: pricingFor(snap, b),
			})
		}
		sort.Slice(out.Body.Hosts, func(i, j int) bool { return out.Body.Hosts[i].Host.Name < out.Body.Hosts[j].Host.Name })
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "model_pricing",
		Method:      "GET",
		Path:        "/models/{ref}/pricing",
		Summary:     "List this model's pricing per host",
		Tags:        []string{"models"},
		Middlewares: protect,
		Errors:      []int{401, 404},
	}, func(ctx context.Context, in *refInput) (*modelPricingOutput, error) {
		snap := d.Catalog.Current()
		m, ok := modelByRef(snap, in.Ref)
		if !ok {
			return nil, huma.Error404NotFound("model not found")
		}
		out := &modelPricingOutput{}
		out.Body.Model = subModel{ID: m.Meta.ID, Name: m.Meta.Name, DisplayName: m.Meta.DisplayName}
		out.Body.Pricing = []modelPricingRow{}
		for _, b := range snap.BindingsForModel(m.Meta.ID) {
			h, ok := snap.Host(b.Spec.HostID)
			if !ok {
				continue
			}
			out.Body.Pricing = append(out.Body.Pricing, modelPricingRow{
				Host:    hostEntryOf(h),
				Pricing: pricingFor(snap, b),
			})
		}
		sort.Slice(out.Body.Pricing, func(i, j int) bool { return out.Body.Pricing[i].Host.Name < out.Body.Pricing[j].Host.Name })
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "host_models",
		Method:      "GET",
		Path:        "/hosts/{ref}/models",
		Summary:     "List the models this host serves, with binding + pricing",
		Tags:        []string{"hosts"},
		Middlewares: protect,
		Errors:      []int{401, 404},
	}, func(ctx context.Context, in *refInput) (*hostModelsOutput, error) {
		snap := d.Catalog.Current()
		h, ok := hostByRef(snap, in.Ref)
		if !ok {
			return nil, huma.Error404NotFound("host not found")
		}
		out := &hostModelsOutput{}
		out.Body.Host = hostEntryOf(h)
		out.Body.Models = []hostModelRow{}
		for _, b := range snap.AllBindings() {
			if b.Spec.HostID != h.Meta.ID {
				continue
			}
			m, ok := snap.Model(b.Spec.ModelID)
			if !ok {
				continue
			}
			out.Body.Models = append(out.Body.Models, hostModelRow{
				Model:   modelEntryOf(m),
				Binding: bindingEntryOf(b),
				Pricing: pricingFor(snap, b),
			})
		}
		sort.Slice(out.Body.Models, func(i, j int) bool { return out.Body.Models[i].Model.Name < out.Body.Models[j].Model.Name })
		return out, nil
	})
}

type modelHostRow struct {
	Host    subHost     `json:"host"`
	Binding subBinding  `json:"binding"`
	Pricing *subPricing `json:"pricing"`
}
type modelHostsOutput struct {
	Body struct {
		Model subModel       `json:"model"`
		Hosts []modelHostRow `json:"hosts"`
	}
}

type modelPricingRow struct {
	Host    subHost     `json:"host"`
	Pricing *subPricing `json:"pricing"`
}
type modelPricingOutput struct {
	Body struct {
		Model   subModel          `json:"model"`
		Pricing []modelPricingRow `json:"pricing"`
	}
}

type hostModelRow struct {
	Model   subModel    `json:"model"`
	Binding subBinding  `json:"binding"`
	Pricing *subPricing `json:"pricing"`
}
type hostModelsOutput struct {
	Body struct {
		Host   subHost        `json:"host"`
		Models []hostModelRow `json:"models"`
	}
}

// ── resolution + mapping helpers ────────────────────────────────────────────

func modelByRef(snap *catalog.Snapshot, ref string) (*model.Model, bool) {
	if m, ok := snap.Model(ref); ok {
		return m, true
	}
	if ms := snap.ModelsByName(ref); len(ms) > 0 {
		return ms[0], true
	}
	return nil, false
}

func hostByRef(snap *catalog.Snapshot, ref string) (*host.Host, bool) {
	if h, ok := snap.Host(ref); ok {
		return h, true
	}
	return snap.HostByName(ref)
}

func hostEntryOf(h *host.Host) subHost {
	return subHost{ID: h.Meta.ID, Name: h.Meta.Name, DisplayName: h.Meta.DisplayName, BaseURL: h.Spec.BaseURL}
}

func modelEntryOf(m *model.Model) subModel {
	return subModel{ID: m.Meta.ID, Name: m.Meta.Name, DisplayName: m.Meta.DisplayName}
}

func bindingEntryOf(b *binding.Binding) subBinding {
	return subBinding{
		ID:           b.Meta.ID,
		Adapter:      string(b.Spec.Adapter),
		UpstreamName: b.Spec.UpstreamName,
		Enabled:      b.IsEnabled(),
		Snapshots:    b.Spec.Snapshots,
	}
}

// pricingFor resolves the pricing for a binding: its explicit PricingID first,
// then the pricing that owns the (model, host) pair. nil when unpriced.
func pricingFor(snap *catalog.Snapshot, b *binding.Binding) *subPricing {
	var p *pricing.Pricing
	if b.Spec.PricingID != "" {
		if pr, ok := snap.Pricing(b.Spec.PricingID); ok {
			p = pr
		}
	}
	if p == nil {
		if pr, ok := snap.PriceByModelHost(b.Spec.ModelID, b.Spec.HostID); ok {
			p = pr
		}
	}
	if p == nil {
		return nil
	}
	out := &subPricing{ID: p.Meta.ID, Name: p.Meta.Name, Currency: p.Spec.Currency, Rates: make([]subRate, 0, len(p.Spec.Rates))}
	for _, r := range p.Spec.Rates {
		out.Rates = append(out.Rates, subRate{Meter: string(r.Meter), Unit: string(r.Unit), Amount: r.Amount, AboveTokens: r.AboveTokens})
	}
	return out
}
