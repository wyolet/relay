package manifest

import (
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/meta"
)

func ToProvider(d ProviderDTO, _ Resolver) (*provider.Provider, error) {
	p := &provider.Provider{
		Meta: d.Metadata.ToMeta(),
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

func ToHost(d HostDTO, _ Resolver) (*host.Host, error) {
	h := &host.Host{
		Meta: d.Metadata.ToMeta(),
		Spec: host.Spec{
			BaseURL:           d.Spec.BaseURL,
			Path:              d.Spec.Path,
			Backend:           d.Spec.Backend,
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

// ToModel resolves the model's owning provider name to an id.
//
// Provider owner: the wire form stores the provider *name* in Metadata.Owner.ID
// when coming from YAML. Callers who need name→id resolution for the owner
// should do so before this call, or supply the provider id directly.
func ToModel(d ModelDTO, idx Resolver) (*model.Model, error) {
	m := &model.Model{
		Meta: d.Metadata.ToMeta(),
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

	var deprecation *model.Deprecation
	if d.Spec.Deprecation != nil {
		dep := *d.Spec.Deprecation
		if dep.Replacement != "" {
			id, ok := idx.ModelID(dep.Replacement)
			if !ok {
				return nil, RefNotFound("model %q: deprecation.replacement: model %q not found", d.Metadata.Name, dep.Replacement)
			}
			dep.Replacement = id
		}
		deprecation = &dep
	}

	m.Spec = model.Spec{
		Family:               d.Spec.Family,
		ProductLine:          d.Spec.ProductLine,
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
		Deprecation:          deprecation,
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

// resolveHostOwner rewrites a host-kind owner from the wire name to its id.
func resolveHostOwner(o *meta.Owner, idx Resolver) {
	if o.Kind == meta.OwnerHost && o.ID != "" {
		if id, ok := idx.HostID(o.ID); ok {
			o.ID = id
		}
	}
}
