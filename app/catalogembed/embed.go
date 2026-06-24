// Package catalogembed composes manifest YAML into the SDK catalog embed schema.
// Server-module only — imports app/* and writes sdk/catalog/catalog.json.
package catalogembed

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
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

// SDKAdapters is the adapter-name set the public SDK client supports today.
var SDKAdapters = map[string]struct{}{
	"openai":           {},
	"openai_responses": {},
	"anthropic":        {},
	"gemini":           {},
}

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

func flatten(snap *catalog.Snapshot, at time.Time) *sdkcatalog.Catalog {
	version := "relay-catalog@" + strings.TrimPrefix(manifest.APIVersion, "relay.wyolet.dev/")
	out := &sdkcatalog.Catalog{
		Version:     version,
		GeneratedAt: at.UTC(),
	}
	for _, h := range snap.EnabledHosts() {
		hostEntry := sdkcatalog.Host{
			Name:          h.Meta.Name,
			BaseURL:       h.Spec.BaseURL,
			DisplayName:   h.Meta.DisplayName,
			Description:   h.Meta.Description,
			HomepageURL:   h.Spec.HomepageURL,
			DocsURL:       h.Spec.DocsURL,
			ConsoleURL:    h.Spec.ConsoleURL,
			StatusPageURL: h.Spec.StatusPageURL,
			Icon:          iconPath(h.Spec.Icon),
		}
		for _, m := range snap.EnabledModels() {
			provSlug, _ := snap.ProviderSlug(m.Meta.Owner.ID)
			var providers []string
			if provSlug != "" {
				providers = []string{provSlug}
			}
			for _, hb := range snap.BindingsForModel(m.Meta.ID) {
				if hb.Spec.HostID != h.Meta.ID || !hb.IsEnabled() {
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
					// Declared aliases resolve to the pointer snapshot, so they
					// ride only that row.
					var aliases []string
					if snapEntry.Name == m.Spec.Pointer && len(m.Spec.Aliases) > 0 {
						aliases = append([]string(nil), m.Spec.Aliases...)
					}
					hostEntry.Models = append(hostEntry.Models, sdkcatalog.Binding{
						MetadataName: snapEntry.Name,
						Adapter:      string(hb.Spec.Adapter),
						Name:         snapEntry.Upstream(),
						Providers:    providers,
						// Featured marks only the family's pointer snapshot, so a
						// featured family yields one shortlist entry, not every
						// dated snapshot.
						Featured: m.Meta.Labels["featured"] == "true" && snapEntry.Name == m.Spec.Pointer,
						Pricing:  rates,
						Aliases:  aliases,
					})
				}
			}
		}
		if len(hostEntry.Models) == 0 {
			continue
		}
		sort.Slice(hostEntry.Models, func(i, j int) bool {
			mi, mj := hostEntry.Models[i], hostEntry.Models[j]
			if mi.MetadataName != mj.MetadataName {
				return mi.MetadataName < mj.MetadataName
			}
			return mi.Adapter < mj.Adapter
		})
		out.Hosts = append(out.Hosts, hostEntry)
	}

	// Rich-metadata sidecars: one ModelInfo per served snapshot slug, one
	// ProviderInfo per author of a served model. Gated on `served` so the
	// sidecars cover exactly the slugs the bindings reference — no orphans.
	served := map[string]bool{}
	for _, h := range out.Hosts {
		for _, b := range h.Models {
			served[b.MetadataName] = true
		}
	}
	seen := map[string]bool{}
	providerIDs := map[string]bool{}
	for _, m := range snap.EnabledModels() {
		provSlug, _ := snap.ProviderSlug(m.Meta.Owner.ID)
		for i := range m.Spec.Snapshots {
			sn := &m.Spec.Snapshots[i]
			if !served[sn.Name] || seen[sn.Name] {
				continue
			}
			seen[sn.Name] = true
			out.Models = append(out.Models, modelInfoFrom(m, sn, provSlug))
			if m.Meta.Owner.ID != "" {
				providerIDs[m.Meta.Owner.ID] = true
			}
		}
	}
	for id := range providerIDs {
		if p, ok := snap.Provider(id); ok {
			out.Providers = append(out.Providers, providerInfoFrom(p))
		}
	}
	return out
}

func iconPath(ic *meta.Icon) string {
	if ic != nil {
		return ic.Path
	}
	return ""
}

// capsFrom copies the domain capability bag into the SDK mirror via a json
// round-trip — both carry identical tags, so this stays correct as flags are
// added without a field-by-field map the generator must remember to update.
func capsFrom(c model.Capabilities) sdkcatalog.Capabilities {
	var out sdkcatalog.Capabilities
	b, _ := json.Marshal(c)
	_ = json.Unmarshal(b, &out)
	return out
}

// modelInfoFrom builds the sidecar for one served snapshot. The rich fields are
// family-level (shared by every snapshot of the model); MetadataName is the
// snapshot slug the bindings reference, and ReleaseDate prefers the snapshot's
// own date when it has one.
func modelInfoFrom(m *model.Model, sn *model.Snapshot, provSlug string) sdkcatalog.ModelInfo {
	releaseDate := m.Spec.ReleaseDate
	if sn.ReleasedAt != "" {
		releaseDate = sn.ReleasedAt
	}
	return sdkcatalog.ModelInfo{
		MetadataName:         sn.Name,
		Provider:             provSlug,
		DisplayName:          m.Meta.DisplayName,
		Description:          m.Meta.Description,
		Family:               m.Spec.Family,
		Version:              m.Spec.Version,
		Capabilities:         capsFrom(m.Spec.Capabilities),
		Modalities:           sdkcatalog.Modalities{Input: m.Spec.Modalities.Input, Output: m.Spec.Modalities.Output},
		ContextWindowInput:   m.Spec.ContextWindowInput,
		ContextWindowOutput:  m.Spec.ContextWindowOutput,
		ContextWindowTotal:   m.Spec.ContextWindowTotal,
		MaxOutputTokens:      m.Spec.MaxOutputTokens,
		KnowledgeCutoff:      m.Spec.KnowledgeCutoff,
		ReleaseDate:          releaseDate,
		License:              m.Spec.License,
		Tags:                 m.Spec.Tags,
		Documentation:        m.Spec.Documentation,
		ProviderModelPageURL: m.Spec.ProviderModelPageURL,
	}
}

func providerInfoFrom(p *provider.Provider) sdkcatalog.ProviderInfo {
	return sdkcatalog.ProviderInfo{
		Name:          p.Meta.Name,
		DisplayName:   p.Meta.DisplayName,
		Description:   p.Meta.Description,
		HomepageURL:   p.Spec.HomepageURL,
		DocsURL:       p.Spec.DocsURL,
		StatusPageURL: p.Spec.StatusPageURL,
		Icon:          iconPath(p.Spec.Icon),
	}
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
				return fmt.Errorf("catalog-embed: host %q model %q: unknown adapter %q", h.Name, b.MetadataName, b.Adapter)
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
			if models[a].MetadataName != models[b].MetadataName {
				return models[a].MetadataName < models[b].MetadataName
			}
			return models[a].Adapter < models[b].Adapter
		})
		cp.Hosts[i].Models = models
	}
	cp.Models = append([]sdkcatalog.ModelInfo(nil), c.Models...)
	sort.Slice(cp.Models, func(a, b int) bool { return cp.Models[a].MetadataName < cp.Models[b].MetadataName })
	cp.Providers = append([]sdkcatalog.ProviderInfo(nil), c.Providers...)
	sort.Slice(cp.Providers, func(a, b int) bool { return cp.Providers[a].Name < cp.Providers[b].Name })
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
