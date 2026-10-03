package catalogembed

import (
	"fmt"
	"time"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	sdkcatalog "github.com/wyolet/relay/sdk/catalog"
)

// Compose loads manifest documents into a catalog snapshot and flattens it to
// the SDK embed schema. Cross-refs resolve via minted ids (no Postgres).
func Compose(docs []manifest.Document, generatedAt time.Time) (*sdkcatalog.Catalog, error) {
	idx := newIndex()
	var (
		provDocs    []*manifest.ProviderDTO
		hostDocs    []*manifest.HostDTO
		mDocs       []*manifest.ModelDTO
		prDocs      []*manifest.PricingDTO
		bindingDocs []*manifest.HostBindingDTO
	)
	for _, d := range docs {
		switch {
		case d.Provider != nil:
			provDocs = append(provDocs, d.Provider)
		case d.Host != nil:
			hostDocs = append(hostDocs, d.Host)
		case d.Model != nil:
			mDocs = append(mDocs, d.Model)
		case d.Pricing != nil:
			prDocs = append(prDocs, d.Pricing)
		case d.HostBinding != nil:
			bindingDocs = append(bindingDocs, d.HostBinding)
		}
	}

	mintIDs(idx.Providers, provDocs, func(d *manifest.ProviderDTO) string { return d.Metadata.Name })
	mintIDs(idx.Hosts, hostDocs, func(d *manifest.HostDTO) string { return d.Metadata.Name })
	mintIDs(idx.Models, mDocs, func(d *manifest.ModelDTO) string { return d.Metadata.Name })
	mintIDs(idx.Pricings, prDocs, func(d *manifest.PricingDTO) string { return d.Metadata.Name })
	mintIDs(idx.Bindings, bindingDocs, func(d *manifest.HostBindingDTO) string { return d.Metadata.Name })

	var (
		provs []*provider.Provider
	)
	for _, d := range provDocs {
		p, err := manifest.ToProvider(*d, idx)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", d.Metadata.Name, err)
		}
		p.Meta.ID = idx.Providers[d.Metadata.Name]
		provs = append(provs, p)
	}
	var hosts []*host.Host
	for _, d := range hostDocs {
		h, err := manifest.ToHost(*d, idx)
		if err != nil {
			return nil, fmt.Errorf("host %q: %w", d.Metadata.Name, err)
		}
		h.Meta.ID = idx.Hosts[d.Metadata.Name]
		hosts = append(hosts, h)
	}
	var models []*model.Model
	for _, d := range mDocs {
		m, err := manifest.ToModel(*d, idx)
		if err != nil {
			return nil, fmt.Errorf("model %q: %w", d.Metadata.Name, err)
		}
		m.Meta.ID = idx.Models[d.Metadata.Name]
		models = append(models, m)
	}
	var pricings []*pricing.Pricing
	for _, d := range prDocs {
		p, err := manifest.ToPricing(*d, idx)
		if err != nil {
			return nil, fmt.Errorf("pricing %q: %w", d.Metadata.Name, err)
		}
		p.Meta.ID = idx.Pricings[d.Metadata.Name]
		pricings = append(pricings, p)
	}
	var bindings []*binding.Binding
	for _, d := range bindingDocs {
		b, err := manifest.ToHostBinding(*d, idx)
		if err != nil {
			return nil, fmt.Errorf("hostbinding %q: %w", d.Metadata.Name, err)
		}
		b.Meta.ID = idx.Bindings[d.Metadata.Name]
		bindings = append(bindings, b)
	}

	snap := catalog.Build(provs, hosts, nil, nil, models, nil, nil, pricings, bindings)
	return flatten(snap, generatedAt), nil
}

type embedIndex struct {
	manifest.MapResolver
}

func newIndex() *embedIndex {
	return &embedIndex{
		MapResolver: manifest.MapResolver{
			Providers:  map[string]string{},
			Hosts:      map[string]string{},
			Policies:   map[string]string{},
			Models:     map[string]string{},
			HostKeys:   map[string]string{},
			RateLimits: map[string]string{},
			Pricings:   map[string]string{},
			Bindings:   map[string]string{},
		},
	}
}

func mintIDs[T any](into map[string]string, docs []T, name func(T) string) {
	for _, d := range docs {
		n := name(d)
		if _, ok := into[n]; !ok {
			into[n] = meta.NewID()
		}
	}
}
