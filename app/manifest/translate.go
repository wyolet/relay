package manifest

import (
	"github.com/wyolet/relay/pkg/catalogkind/host"
	catalogmanifest "github.com/wyolet/relay/pkg/catalogkind/manifest"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/meta"
)

// ---------------------------------------------------------------------------
// Provider
// ---------------------------------------------------------------------------

func ToProvider(d ProviderDTO, idx Resolver) (*provider.Provider, error) {
	return catalogmanifest.ToProvider(d, idx)
}

func FromProvider(p *provider.Provider, _ ReverseResolver) ProviderDTO {
	return ProviderDTO{
		APIVersion: APIVersion,
		Kind:       "Provider",
		Metadata:   metaToWire(p.Meta),
		Spec: ProviderSpec{
			Enabled:       p.Spec.Enabled,
			HomepageURL:   p.Spec.HomepageURL,
			DocsURL:       p.Spec.DocsURL,
			StatusPageURL: p.Spec.StatusPageURL,
			Icon:          p.Spec.Icon,
		},
	}
}

// ---------------------------------------------------------------------------
// Host
// ---------------------------------------------------------------------------

func ToHost(d HostDTO, idx Resolver) (*host.Host, error) {
	return catalogmanifest.ToHost(d, idx)
}

func FromHost(h *host.Host, _ ReverseResolver) HostDTO {
	return HostDTO{
		APIVersion: APIVersion,
		Kind:       "Host",
		Metadata:   metaToWire(h.Meta),
		Spec: HostSpec{
			BaseURL:           h.Spec.BaseURL,
			Path:              h.Spec.Path,
			Backend:           h.Spec.Backend,
			NoAuth:            h.Spec.NoAuth,
			PricingStrategies: h.Spec.PricingStrategies,
			Enabled:           h.Spec.Enabled,
			HomepageURL:       h.Spec.HomepageURL,
			DocsURL:           h.Spec.DocsURL,
			ConsoleURL:        h.Spec.ConsoleURL,
			StatusPageURL:     h.Spec.StatusPageURL,
			Icon:              h.Spec.Icon,
		},
	}
}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

// ToModel resolves the model's owning provider name to an id.
func ToModel(d ModelDTO, idx Resolver) (*model.Model, error) {
	return catalogmanifest.ToModel(d, idx)
}

func FromModel(m *model.Model, rev ReverseResolver) ModelDTO {
	wm := metaToWire(m.Meta)
	// Render owner provider id → name
	if m.Meta.Owner.Kind == meta.OwnerProvider && m.Meta.Owner.ID != "" {
		if pname, ok := rev.ProviderName(m.Meta.Owner.ID); ok {
			wm.Owner.Name = pname
		}
	}
	deprecation := m.Spec.Deprecation
	if deprecation != nil && deprecation.Replacement != "" {
		if name, ok := rev.ModelName(deprecation.Replacement); ok {
			dep := *deprecation
			dep.Replacement = name
			deprecation = &dep
		}
	}

	return ModelDTO{
		APIVersion: APIVersion,
		Kind:       "Model",
		Metadata:   wm,
		Spec: ModelSpec{
			Family:               m.Spec.Family,
			ProductLine:          m.Spec.ProductLine,
			Version:              m.Spec.Version,
			Capabilities:         m.Spec.Capabilities,
			Modalities:           m.Spec.Modalities,
			ContextWindowInput:   m.Spec.ContextWindowInput,
			ContextWindowOutput:  m.Spec.ContextWindowOutput,
			ContextWindowTotal:   m.Spec.ContextWindowTotal,
			MaxOutputTokens:      m.Spec.MaxOutputTokens,
			KnowledgeCutoff:      m.Spec.KnowledgeCutoff,
			ReleaseDate:          m.Spec.ReleaseDate,
			DeprecationDate:      m.Spec.DeprecationDate,
			Deprecation:          deprecation,
			Tags:                 m.Spec.Tags,
			Documentation:        m.Spec.Documentation,
			License:              m.Spec.License,
			ProviderModelPageURL: m.Spec.ProviderModelPageURL,
			Enabled:              m.Spec.Enabled,
			Snapshots:            m.Spec.Snapshots,
			Pointer:              m.Spec.Pointer,
			Aliases:              m.Spec.Aliases,
		},
	}
}
