package catalogembed

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	sdkcatalog "github.com/wyolet/relay/sdk/catalog"
)

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
