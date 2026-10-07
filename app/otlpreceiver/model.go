package otlpreceiver

import (
	"strings"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/pkg/otlp"
	"github.com/wyolet/relay/pkg/slug"
)

// ProviderHint says what a provider name in telemetry refers to in the catalog. Telemetry conventions name providers their own way, and often name the service that served the call rather than the company whose model it is. Either field may be empty.
type ProviderHint struct {
	// Provider is the slug of the catalog provider whose models are reported under the name.
	Provider string
	// Host is the slug of the catalog host that serves calls reported under the name; its pricing applies to them.
	Host string
}

// catalogModel is the catalog's view of a reported model: its identity and the rate sheet a reported call is priced against. The zero value is a model the catalog does not know, which leaves the event unpriced.
type catalogModel struct {
	id          string
	name        string
	provider    string
	pricingID   string
	pricingName string
}

// resolveModel finds the catalog model a reported call ran on. The response model is tried before the request model because it names what actually served the call. Each is tried qualified by the hinted provider, then by the provider as reported, then bare, since a bare name may be served by more than one provider.
func resolveModel(snap *appcatalog.Snapshot, inf otlp.Inference, hint ProviderHint) catalogModel {
	if snap == nil {
		return catalogModel{}
	}
	for _, name := range []string{inf.ResponseModel, inf.RequestModel} {
		if name == "" {
			continue
		}
		var refs []string
		if hint.Provider != "" {
			refs = append(refs, hint.Provider+"/"+name)
		}
		if inf.Provider != "" && inf.Provider != hint.Provider {
			refs = append(refs, inf.Provider+"/"+name)
		}
		refs = append(refs, name)
		for _, ref := range refs {
			if m, hostID, ok := lookup(snap, ref); ok {
				out := catalogModel{id: m.Meta.ID, name: m.Meta.Name}
				out.provider, _ = snap.ProviderSlug(m.Meta.Owner.ID)
				if p, ok := pricingFor(snap, m, hostID, inf.Host, hint.Host, out.provider); ok {
					out.pricingID, out.pricingName = p.Meta.ID, p.Meta.Name
				}
				return out
			}
		}
	}
	return catalogModel{}
}

// lookup resolves a model reference the way routing does: exact catalog names first, declared aliases last.
func lookup(snap *appcatalog.Snapshot, ref string) (*model.Model, string, bool) {
	key := slug.From(ref)
	if key == "" {
		return nil, "", false
	}
	if m, _, hostID, ok := snap.ResolveSnapshot(key); ok {
		return m, hostID, true
	}
	if a, ok := snap.ResolveAlias(key, !strings.ContainsRune(ref, '@')); ok {
		return a.Model, a.HostID, true
	}
	return nil, "", false
}

// pricingFor picks the rate sheet for a call relay did not route, so no binding was chosen for it: the host the reference pinned, else the host the client reported, else the hinted host, else the provider's own host for its model, else the first enabled binding that has pricing. A host slug with no priced binding for the model falls through to the next.
func pricingFor(snap *appcatalog.Snapshot, m *model.Model, pinnedHostID, reportedHost, hintedHost, providerSlug string) (*pricing.Pricing, bool) {
	var hinted, own, first *pricing.Pricing
	for _, b := range snap.BindingsForModel(m.Meta.ID) {
		if !b.IsEnabled() {
			continue
		}
		p, ok := snap.PricingForBinding(b)
		if !ok {
			continue
		}
		if pinnedHostID != "" {
			if b.Spec.HostID == pinnedHostID {
				return p, true
			}
			continue
		}
		hostSlug, _ := snap.HostSlug(b.Spec.HostID)
		if hostSlug != "" && hostSlug == reportedHost {
			return p, true
		}
		if hinted == nil && hostSlug != "" && hostSlug == hintedHost {
			hinted = p
		}
		if own == nil && hostSlug != "" && hostSlug == providerSlug {
			own = p
		}
		if first == nil {
			first = p
		}
	}
	switch {
	case hinted != nil:
		return hinted, true
	case own != nil:
		return own, true
	}
	return first, first != nil
}
