package control

import (
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
)

func subresSnap() (*catalog.Snapshot, string, string) {
	provID, hostID, modID := meta.NewID(), meta.NewID(), meta.NewID()
	pricingID := meta.NewID()

	prov := &provider.Provider{Meta: meta.Metadata{ID: provID, Name: "openai", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	h := &host.Host{Meta: meta.Metadata{ID: hostID, Name: "openai", DisplayName: "OpenAI", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "https://api.openai.com"}}
	m := &model.Model{
		Meta: meta.Metadata{ID: modID, Name: "gpt-4o", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "gpt-4o"}}, Pointer: "gpt-4o"},
	}
	pr := &pricing.Pricing{
		Meta: meta.Metadata{ID: pricingID, Name: "openai-gpt-4o", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}},
		Spec: pricing.Spec{Currency: "USD", TargetModelIDs: []string{modID}, Rates: []pricing.Rate{{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: 2.5}}},
	}
	b := &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "gpt-4o-on-openai", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: modID, HostID: hostID, Adapter: adapters.OpenAI, PricingID: pricingID},
	}
	snap := catalog.Build(
		[]*provider.Provider{prov}, []*host.Host{h}, nil, nil,
		[]*model.Model{m}, nil, nil, []*pricing.Pricing{pr}, []*binding.Binding{b},
	)
	return snap, modID, hostID
}

func TestSubresources_ModelHostsAndPricing(t *testing.T) {
	snap, _, hostID := subresSnap()

	m, ok := modelByRef(snap, "gpt-4o")
	if !ok {
		t.Fatal("modelByRef by slug failed")
	}

	bnds := snap.BindingsForModel(m.Meta.ID)
	if len(bnds) != 1 {
		t.Fatalf("BindingsForModel = %d, want 1", len(bnds))
	}
	b := bnds[0]
	if b.Spec.HostID != hostID {
		t.Errorf("binding host = %q, want %q", b.Spec.HostID, hostID)
	}

	// pricing resolves via the binding's explicit PricingID and embeds rates.
	p := pricingFor(snap, b)
	if p == nil {
		t.Fatal("pricingFor returned nil; want resolved pricing")
	}
	if p.Name != "openai-gpt-4o" || p.Currency != "USD" || len(p.Rates) != 1 {
		t.Fatalf("pricing = %+v, want openai-gpt-4o/USD/1 rate", p)
	}
	if p.Rates[0].Meter != "tokens.input" || p.Rates[0].Amount != 2.5 {
		t.Errorf("rate = %+v, want tokens.input/2.5", p.Rates[0])
	}

	// host resolves and enriches.
	if h, ok := hostByRef(snap, "openai"); !ok || h.Meta.ID != hostID {
		t.Errorf("hostByRef by slug failed: ok=%v", ok)
	}
}

func TestSubresources_UnpricedBindingYieldsNil(t *testing.T) {
	provID, hostID, modID := meta.NewID(), meta.NewID(), meta.NewID()
	prov := &provider.Provider{Meta: meta.Metadata{ID: provID, Name: "p", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	h := &host.Host{Meta: meta.Metadata{ID: hostID, Name: "h", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "http://x"}}
	m := &model.Model{Meta: meta.Metadata{ID: modID, Name: "m", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}}, Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "m"}}, Pointer: "m"}}
	b := &binding.Binding{Meta: meta.Metadata{ID: meta.NewID(), Name: "m-on-h", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: binding.Spec{ModelID: modID, HostID: hostID, Adapter: adapters.OpenAI}}
	snap := catalog.Build([]*provider.Provider{prov}, []*host.Host{h}, nil, nil, []*model.Model{m}, nil, nil, nil, []*binding.Binding{b})

	if p := pricingFor(snap, snap.BindingsForModel(modID)[0]); p != nil {
		t.Errorf("unpriced binding should yield nil pricing, got %+v", p)
	}
}
