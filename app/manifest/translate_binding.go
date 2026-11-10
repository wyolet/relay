package manifest

import (
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	catalogmanifest "github.com/wyolet/relay/pkg/catalogkind/manifest"
	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	"github.com/wyolet/relay/pkg/meta"
)

// ---------------------------------------------------------------------------
// Pricing
// ---------------------------------------------------------------------------

// ToPricing resolves the owner host name → id and target model names → ids.
func ToPricing(d PricingDTO, idx Resolver) (*pricing.Pricing, error) {
	return catalogmanifest.ToPricing(d, idx)
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
	return catalogmanifest.ToHostBinding(d, idx)
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
