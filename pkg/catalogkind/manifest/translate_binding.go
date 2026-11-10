package manifest

import (
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	"github.com/wyolet/relay/pkg/meta"
)

// ToPricing resolves the owner host name → id and target model names → ids.
func ToPricing(d PricingDTO, idx Resolver) (*pricing.Pricing, error) {
	m := d.Metadata.ToMeta()
	resolveHostOwner(&m.Owner, idx)

	modelIDs := make([]string, 0, len(d.Spec.TargetModels))
	for _, name := range d.Spec.TargetModels {
		id, ok := idx.ModelID(name)
		if !ok {
			return nil, RefNotFound("pricing %q: targetModels: model %q not found", d.Metadata.Name, name)
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

// ToHostBinding resolves model, host, and (optional) pricing names to ids.
func ToHostBinding(d HostBindingDTO, idx Resolver) (*binding.Binding, error) {
	modelID, ok := idx.ModelID(d.Spec.Model)
	if !ok {
		return nil, RefNotFound("hostbinding %q: model %q not found", d.Metadata.Name, d.Spec.Model)
	}
	hostID, ok := idx.HostID(d.Spec.Host)
	if !ok {
		return nil, RefNotFound("hostbinding %q: host %q not found", d.Metadata.Name, d.Spec.Host)
	}
	var pricingID string
	if d.Spec.Pricing != "" {
		pid, ok := idx.PricingID(d.Spec.Pricing)
		if !ok {
			return nil, RefNotFound("hostbinding %q: pricing %q not found", d.Metadata.Name, d.Spec.Pricing)
		}
		pricingID = pid
	}
	m := d.Metadata.ToMeta()
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
