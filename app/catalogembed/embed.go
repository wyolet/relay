// Package catalogembed composes manifest YAML into the SDK catalog embed schema.
// Server-module only — imports app/* and writes sdk/catalog/catalog.json.
package catalogembed

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	sdkcatalog "github.com/wyolet/relay/sdk/catalog"
)

// SDKAdapters is the adapter-name set the public SDK client supports today.
var SDKAdapters = map[string]struct{}{
	"openai":    {},
	"anthropic": {},
	"gemini":    {},
}

// Compose loads manifest documents into a catalog snapshot and flattens it to
// the SDK embed schema. Cross-refs resolve via minted ids (no Postgres).
func Compose(docs []manifest.Document, generatedAt time.Time) (*sdkcatalog.Catalog, error) {
	idx := newIndex()
	var (
		provDocs []*manifest.ProviderDTO
		hostDocs []*manifest.HostDTO
		mDocs    []*manifest.ModelDTO
		prDocs   []*manifest.PricingDTO
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
		}
	}

	mintIDs(idx.Providers, provDocs, func(d *manifest.ProviderDTO) string { return d.Metadata.Name })
	mintIDs(idx.Hosts, hostDocs, func(d *manifest.HostDTO) string { return d.Metadata.Name })
	mintIDs(idx.Models, mDocs, func(d *manifest.ModelDTO) string { return d.Metadata.Name })
	mintIDs(idx.Pricings, prDocs, func(d *manifest.PricingDTO) string { return d.Metadata.Name })

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

	snap := catalog.Build(provs, hosts, nil, nil, models, nil, nil, pricings, nil)
	return flatten(snap, generatedAt), nil
}

func flatten(snap *catalog.Snapshot, at time.Time) *sdkcatalog.Catalog {
	version := "relay-catalog@" + strings.TrimPrefix(manifest.APIVersion, "relay.wyolet.dev/")
	out := &sdkcatalog.Catalog{
		Version:     version,
		GeneratedAt: at.UTC(),
	}
	for _, h := range snap.EnabledHosts() {
		hostEntry := sdkcatalog.Host{
			Name:    h.Meta.Name,
			BaseURL: h.Spec.BaseURL,
		}
		for _, m := range snap.EnabledModels() {
			provSlug, _ := snap.ProviderSlug(m.Meta.Owner.ID)
			var providers []string
			if provSlug != "" {
				providers = []string{provSlug}
			}
			for _, hb := range m.Spec.Hosts {
				if hb.HostID != h.Meta.ID || !hb.IsEnabled() {
					continue
				}
				for i := range m.Spec.Snapshots {
					snapEntry := &m.Spec.Snapshots[i]
					if !hb.Serves(snapEntry.Name) {
						continue
					}
					var rates []sdkcatalog.Rate
					if pr, ok := snap.PriceByModelHost(m.Meta.ID, h.Meta.ID); ok && pr.IsEnabled() {
						rates = ratesFrom(pr)
					}
					hostEntry.Models = append(hostEntry.Models, sdkcatalog.Binding{
						Model:     snapEntry.Name,
						Adapter:   string(hb.Adapter),
						Upstream:  snapEntry.Upstream(),
						Providers: providers,
						Pricing:   rates,
					})
				}
			}
		}
		if len(hostEntry.Models) == 0 {
			continue
		}
		sort.Slice(hostEntry.Models, func(i, j int) bool {
			mi, mj := hostEntry.Models[i], hostEntry.Models[j]
			if mi.Model != mj.Model {
				return mi.Model < mj.Model
			}
			return mi.Adapter < mj.Adapter
		})
		out.Hosts = append(out.Hosts, hostEntry)
	}
	return out
}

func ratesFrom(p *pricing.Pricing) []sdkcatalog.Rate {
	rates := make([]sdkcatalog.Rate, len(p.Spec.Rates))
	for i, r := range p.Spec.Rates {
		rates[i] = sdkcatalog.Rate{
			Meter:       string(r.Meter),
			Unit:        string(r.Unit),
			Amount:      r.Amount,
			AboveTokens: r.AboveTokens,
		}
	}
	return rates
}

// ValidateAdapters returns an error if any binding uses an adapter name outside
// SDKAdapters.
func ValidateAdapters(c *sdkcatalog.Catalog) error {
	for _, h := range c.Hosts {
		for _, b := range h.Models {
			if _, ok := SDKAdapters[b.Adapter]; !ok {
				return fmt.Errorf("catalog-embed: host %q model %q: unknown adapter %q", h.Name, b.Model, b.Adapter)
			}
		}
	}
	return nil
}

// MarshalJSON encodes c deterministically (sorted hosts and bindings).
func MarshalJSON(c *sdkcatalog.Catalog) ([]byte, error) {
	cp := *c
	cp.Hosts = append([]sdkcatalog.Host(nil), c.Hosts...)
	sort.Slice(cp.Hosts, func(i, j int) bool { return cp.Hosts[i].Name < cp.Hosts[j].Name })
	for i := range cp.Hosts {
		models := append([]sdkcatalog.Binding(nil), cp.Hosts[i].Models...)
		sort.Slice(models, func(a, b int) bool {
			if models[a].Model != models[b].Model {
				return models[a].Model < models[b].Model
			}
			return models[a].Adapter < models[b].Adapter
		})
		cp.Hosts[i].Models = models
	}
	return json.MarshalIndent(&cp, "", "  ")
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
