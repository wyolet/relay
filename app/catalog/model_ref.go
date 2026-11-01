package catalog

import (
	"strings"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/modelref"
	"github.com/wyolet/relay/app/overlay"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/pkg/slug"
)

// BuildCatalog assembles a Snapshot of the catalog kinds alone (providers, hosts, models, pricings, bindings, with overlays merged), for callers that resolve model refs without the tenancy, key or policy rows.
func BuildCatalog(provs []*provider.Provider, hosts []*host.Host, models []*model.Model, pricings []*pricing.Pricing, bindings []*binding.Binding, ovls []*overlay.Overlay) *Snapshot {
	return build(nil, provs, hosts, nil, nil, models, nil, nil, pricings, bindings, ovls, nil, nil, nil, nil, nil, nil, nil)
}

// ResolvedRef is a caller's model ref resolved against the snapshot's name and alias indices.
type ResolvedRef struct {
	Models   []*model.Model
	Snapshot *model.Snapshot
	// PinHostID is set when the ref named a host ("model@host"); binding selection is then limited to it.
	PinHostID string
	// Alias is the declared alias that matched, nil for a real catalog name.
	Alias *AliasRef
}

// ResolveModelRef maps a caller-supplied model ref to its snapshot: the slug-normalised name first, declared aliases only after every real name missed. Wildcard alias patterns are skipped for refs carrying an "@host" pin, whose pin segment would corrupt the match.
func (s *Snapshot) ResolveModelRef(name string) ResolvedRef {
	key := slug.From(name)
	if key == "" {
		return ResolvedRef{}
	}
	if m, sn, hostID, ok := s.ResolveSnapshot(key); ok {
		return ResolvedRef{Models: []*model.Model{m}, Snapshot: sn, PinHostID: hostID}
	}
	if ref, ok := s.ResolveAlias(key, !strings.ContainsRune(name, '@')); ok {
		return ResolvedRef{Models: []*model.Model{ref.Model}, Snapshot: ref.Snapshot, PinHostID: ref.HostID, Alias: &ref}
	}
	return ResolvedRef{}
}

// AliasUpstream returns the usage tag and the wire model name an alias match imposes: an exact alias sends its declared string, a pattern sends the caller's raw string. Both are empty when ref is nil.
func AliasUpstream(ref *AliasRef, raw string) (via, upstream string) {
	if ref == nil {
		return "", ""
	}
	if ref.Pattern {
		return "alias:" + ref.Name, raw
	}
	return "alias:" + ref.Name, ref.Name
}

// RefsGrant reports whether any ref grants (provider, model, host) under the rules policy allow sets use: with hideDeprecated, a wildcard-model match does not grant, while a ref naming the model still does.
func RefsGrant(refs []modelref.Ref, providerSlug, modelSlug, hostSlug string, hideDeprecated bool) bool {
	return refsGrant(refs, providerSlug, modelSlug, hostSlug, hideDeprecated)
}

// ModelDeprecated reports whether m is deprecated or sunset, which hides it from wildcard grants.
func ModelDeprecated(m *model.Model) bool { return modelDeprecated(m) }
