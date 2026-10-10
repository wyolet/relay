package manifest

import (
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/pricing"
)

// ---------------------------------------------------------------------------
// Pricing
// ---------------------------------------------------------------------------

// ToPricing resolves the owner host name → id and target model names → ids.
func ToPricing(d PricingDTO, idx Resolver) (*pricing.Pricing, error) {
	m := d.Metadata.toMeta()
	if m.Owner.Kind == meta.OwnerHost && m.Owner.ID != "" {
		if hid, ok := idx.HostID(m.Owner.ID); ok {
			m.Owner.ID = hid
		}
	}

	modelIDs := make([]string, 0, len(d.Spec.TargetModels))
	for _, name := range d.Spec.TargetModels {
		id, ok := idx.ModelID(name)
		if !ok {
			return nil, refNotFound("pricing %q: targetModels: model %q not found", d.Metadata.Name, name)
		}
		modelIDs = append(modelIDs, id)
	}

	rates := make([]pricing.Rate, 0, len(d.Spec.Rates))
	for _, r := range d.Spec.Rates {
		rates = append(rates, pricing.Rate{
			Meter:       pricing.Meter(r.Meter),
			Unit:        pricing.Unit(r.Unit),
			Amount:      r.Amount,
			AboveTokens: r.AboveTokens,
			ServiceTier: r.ServiceTier,
		})
	}

	return &pricing.Pricing{
		Meta: m,
		Spec: pricing.Spec{
			Currency:       d.Spec.Currency,
			TargetModelIDs: modelIDs,
			Rates:          rates,
			Enabled:        d.Spec.Enabled,
		},
	}, nil
}

func FromPricing(p *pricing.Pricing, rev ReverseResolver) PricingDTO {
	wm := metaToWire(p.Meta)
	if p.Meta.Owner.Kind == meta.OwnerHost && p.Meta.Owner.ID != "" {
		if hname, ok := rev.HostName(p.Meta.Owner.ID); ok {
			wm.Owner.Name = hname
		}
	}

	models := make([]string, 0, len(p.Spec.TargetModelIDs))
	for _, id := range p.Spec.TargetModelIDs {
		name, _ := rev.ModelName(id)
		if name == "" {
			name = id
		}
		models = append(models, name)
	}

	rates := make([]PricingRateDTO, 0, len(p.Spec.Rates))
	for _, r := range p.Spec.Rates {
		rates = append(rates, PricingRateDTO{
			Meter:       string(r.Meter),
			Unit:        string(r.Unit),
			Amount:      r.Amount,
			AboveTokens: r.AboveTokens,
			ServiceTier: r.ServiceTier,
		})
	}

	return PricingDTO{
		APIVersion: APIVersion,
		Kind:       "Pricing",
		Metadata:   wm,
		Spec: PricingSpec{
			Currency:     p.Spec.Currency,
			TargetModels: models,
			Rates:        rates,
			Enabled:      p.Spec.Enabled,
		},
	}
}

// ---------------------------------------------------------------------------
// HostBinding
// ---------------------------------------------------------------------------

// ToHostBinding resolves model, host, and (optional) pricing names to ids.
func ToHostBinding(d HostBindingDTO, idx Resolver) (*binding.Binding, error) {
	modelID, ok := idx.ModelID(d.Spec.Model)
	if !ok {
		return nil, refNotFound("hostbinding %q: model %q not found", d.Metadata.Name, d.Spec.Model)
	}
	hostID, ok := idx.HostID(d.Spec.Host)
	if !ok {
		return nil, refNotFound("hostbinding %q: host %q not found", d.Metadata.Name, d.Spec.Host)
	}
	var pricingID string
	if d.Spec.Pricing != "" {
		pid, ok := idx.PricingID(d.Spec.Pricing)
		if !ok {
			return nil, refNotFound("hostbinding %q: pricing %q not found", d.Metadata.Name, d.Spec.Pricing)
		}
		pricingID = pid
	}
	m := d.Metadata.toMeta()
	if m.Owner.Kind == "" {
		m.Owner.Kind = meta.OwnerSystem
	}
	return &binding.Binding{
		Meta: m,
		Spec: binding.Spec{
			ModelID:      modelID,
			HostID:       hostID,
			Adapter:      adapters.Name(d.Spec.Adapter),
			UpstreamName: d.Spec.UpstreamName,
			PricingID:    pricingID,
			Enabled:      d.Spec.Enabled,
			Snapshots:    d.Spec.Snapshots,
		},
	}, nil
}

func FromHostBinding(b *binding.Binding, rev ReverseResolver) HostBindingDTO {
	modelName, _ := rev.ModelName(b.Spec.ModelID)
	if modelName == "" {
		modelName = b.Spec.ModelID
	}
	hostName, _ := rev.HostName(b.Spec.HostID)
	if hostName == "" {
		hostName = b.Spec.HostID
	}
	pricingName := ""
	if b.Spec.PricingID != "" {
		n, _ := rev.PricingName(b.Spec.PricingID)
		if n == "" {
			n = b.Spec.PricingID
		}
		pricingName = n
	}
	return HostBindingDTO{
		APIVersion: APIVersion,
		Kind:       "HostBinding",
		Metadata:   metaToWire(b.Meta),
		Spec: HostBindingSpec{
			Model:        modelName,
			Host:         hostName,
			Adapter:      string(b.Spec.Adapter),
			UpstreamName: b.Spec.UpstreamName,
			Pricing:      pricingName,
			Enabled:      b.Spec.Enabled,
			Snapshots:    b.Spec.Snapshots,
		},
	}
}
