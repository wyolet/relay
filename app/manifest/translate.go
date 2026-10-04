package manifest

import (
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/provider"
)

// ---------------------------------------------------------------------------
// Provider
// ---------------------------------------------------------------------------

func ToProvider(d ProviderDTO, _ Resolver) (*provider.Provider, error) {
	p := &provider.Provider{
		Meta: d.Metadata.toMeta(),
		Spec: provider.Spec{
			Enabled:       d.Spec.Enabled,
			HomepageURL:   d.Spec.HomepageURL,
			DocsURL:       d.Spec.DocsURL,
			StatusPageURL: d.Spec.StatusPageURL,
			Icon:          d.Spec.Icon,
		},
	}
	// Default owner.kind to "system" when the wire form left it empty
	// (catalog-supplied providers are system-owned by convention; BYO
	// providers must declare kind: user explicitly).
	if p.Meta.Owner.Kind == "" {
		p.Meta.Owner.Kind = meta.OwnerSystem
	}
	return p, nil
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
	policies := make([]string, 0, len(d.Spec.Policies))
	for _, name := range d.Spec.Policies {
		if id, ok := idx.PolicyID(name); ok {
			policies = append(policies, id)
		} else {
			policies = append(policies, name)
		}
	}
	defaultPolicy := d.Spec.DefaultPolicy
	if defaultPolicy != "" {
		if id, ok := idx.PolicyID(defaultPolicy); ok {
			defaultPolicy = id
		}
	}
	h := &host.Host{
		Meta: d.Metadata.toMeta(),
		Spec: host.Spec{
			BaseURL:           d.Spec.BaseURL,
			Path:              d.Spec.Path,
			Backend:           d.Spec.Backend,
			Policies:          policies,
			DefaultPolicy:     defaultPolicy,
			NoAuth:            d.Spec.NoAuth,
			PricingStrategies: d.Spec.PricingStrategies,
			Enabled:           d.Spec.Enabled,
			HomepageURL:       d.Spec.HomepageURL,
			DocsURL:           d.Spec.DocsURL,
			ConsoleURL:        d.Spec.ConsoleURL,
			StatusPageURL:     d.Spec.StatusPageURL,
			Icon:              d.Spec.Icon,
		},
	}
	// Default owner.kind to "system" when wire form left it empty.
	if h.Meta.Owner.Kind == "" {
		h.Meta.Owner.Kind = meta.OwnerSystem
	}
	return h, nil
}

func FromHost(h *host.Host, rev ReverseResolver) HostDTO {
	policies := make([]string, 0, len(h.Spec.Policies))
	for _, id := range h.Spec.Policies {
		if name, ok := rev.PolicyName(id); ok {
			policies = append(policies, name)
		} else {
			policies = append(policies, id)
		}
	}
	defaultPolicy := h.Spec.DefaultPolicy
	if defaultPolicy != "" {
		if name, ok := rev.PolicyName(defaultPolicy); ok {
			defaultPolicy = name
		}
	}
	return HostDTO{
		APIVersion: APIVersion,
		Kind:       "Host",
		Metadata:   metaToWire(h.Meta),
		Spec: HostSpec{
			BaseURL:           h.Spec.BaseURL,
			Path:              h.Spec.Path,
			Backend:           h.Spec.Backend,
			Policies:          policies,
			DefaultPolicy:     defaultPolicy,
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
//
// Provider owner: the wire form stores the provider *name* in Metadata.Owner.ID
// when coming from YAML. Callers who need name→id resolution for the owner
// should do so before this call, or supply the provider id directly.
func ToModel(d ModelDTO, idx Resolver) (*model.Model, error) {
	m := &model.Model{
		Meta: d.Metadata.toMeta(),
	}

	// Resolve owner provider name → id if the owner kind is provider and the
	// ID looks like a name (not a UUID). We do a best-effort resolution; if it
	// already looks resolved the caller wins.
	ownerID := m.Meta.Owner.ID
	if m.Meta.Owner.Kind == meta.OwnerProvider && ownerID != "" {
		if pid, ok := idx.ProviderID(ownerID); ok {
			m.Meta.Owner.ID = pid
		}
		// else: already an id or caller's responsibility
	}

	m.Spec = model.Spec{
		Family:               d.Spec.Family,
		Version:              d.Spec.Version,
		Capabilities:         d.Spec.Capabilities,
		Modalities:           d.Spec.Modalities,
		ContextWindowInput:   d.Spec.ContextWindowInput,
		ContextWindowOutput:  d.Spec.ContextWindowOutput,
		ContextWindowTotal:   d.Spec.ContextWindowTotal,
		MaxOutputTokens:      d.Spec.MaxOutputTokens,
		KnowledgeCutoff:      d.Spec.KnowledgeCutoff,
		ReleaseDate:          d.Spec.ReleaseDate,
		DeprecationDate:      d.Spec.DeprecationDate,
		Deprecation:          d.Spec.Deprecation,
		Tags:                 d.Spec.Tags,
		Documentation:        d.Spec.Documentation,
		License:              d.Spec.License,
		ProviderModelPageURL: d.Spec.ProviderModelPageURL,
		Enabled:              d.Spec.Enabled,
		Snapshots:            d.Spec.Snapshots,
		Pointer:              d.Spec.Pointer,
		Aliases:              d.Spec.Aliases,
	}
	return m, nil
}

func FromModel(m *model.Model, rev ReverseResolver) ModelDTO {
	wm := metaToWire(m.Meta)
	// Render owner provider id → name
	if m.Meta.Owner.Kind == meta.OwnerProvider && m.Meta.Owner.ID != "" {
		if pname, ok := rev.ProviderName(m.Meta.Owner.ID); ok {
			wm.Owner.Name = pname
		}
	}

	return ModelDTO{
		APIVersion: APIVersion,
		Kind:       "Model",
		Metadata:   wm,
		Spec: ModelSpec{
			Family:               m.Spec.Family,
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
			Deprecation:          m.Spec.Deprecation,
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
