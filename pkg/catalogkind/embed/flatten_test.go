package embed

import (
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/meta"
)

// flattenFixture is one model on two hosts. The first host owns two rate sheets for the pair; its binding names the one billing uses, which is not the one the (model, host) lookup returns.
func flattenFixture() (idx *Index, keptBinding string) {
	provID, modID := meta.NewID(), meta.NewID()
	hostA, hostB := meta.NewID(), meta.NewID()
	coverID, explicitID := meta.NewID(), meta.NewID()
	bindA := meta.NewID()
	sheet := func(id, name string, input float64) *pricing.Pricing {
		return &pricing.Pricing{
			Meta: meta.Metadata{ID: id, Name: name, Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostA}},
			Spec: pricing.Spec{
				Currency:       "USD",
				TargetModelIDs: []string{modID},
				Rates:          []pricing.Rate{{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: input}},
			},
		}
	}
	idx = NewIndex(
		[]*provider.Provider{{Meta: meta.Metadata{ID: provID, Name: "prov", Owner: meta.Owner{Kind: meta.OwnerSystem}}}},
		[]*host.Host{
			{Meta: meta.Metadata{ID: hostA, Name: "host-a", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "http://a.example"}},
			{Meta: meta.Metadata{ID: hostB, Name: "host-b", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "http://b.example"}},
		},
		[]*model.Model{{
			Meta: meta.Metadata{ID: modID, Name: "m", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
			Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "m-snap"}}, Pointer: "m-snap"},
		}},
		// Both sheets target the pair; the later one wins the (model, host) lookup.
		[]*pricing.Pricing{sheet(explicitID, "explicit", 7), sheet(coverID, "cover", 1)},
		[]*binding.Binding{
			{Meta: meta.Metadata{ID: bindA, Name: "on-a", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: binding.Spec{ModelID: modID, HostID: hostA, Adapter: adapters.OpenAI, PricingID: explicitID}},
			{Meta: meta.Metadata{ID: meta.NewID(), Name: "on-b", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: binding.Spec{ModelID: modID, HostID: hostB, Adapter: adapters.OpenAI}},
		},
	)
	return idx, bindA
}

func TestFlatten_KeepFiltersBindings(t *testing.T) {
	idx, kept := flattenFixture()

	all := Flatten(idx, time.Time{}, nil)
	if len(all.Hosts) != 2 {
		t.Fatalf("nil keep: hosts = %d, want 2", len(all.Hosts))
	}

	scoped := Flatten(idx, time.Time{}, func(_ *model.Model, b *binding.Binding) bool { return b.Meta.ID == kept })
	if len(scoped.Hosts) != 1 || scoped.Hosts[0].Name != "host-a" {
		t.Fatalf("hosts = %+v, want host-a alone", scoped.Hosts)
	}
	if len(scoped.Models) != 1 || scoped.Models[0].MetadataName != "m-snap" {
		t.Errorf("models sidecar = %+v, want m-snap", scoped.Models)
	}

	none := Flatten(idx, time.Time{}, func(*model.Model, *binding.Binding) bool { return false })
	if len(none.Hosts) != 0 || len(none.Models) != 0 || len(none.Providers) != 0 {
		t.Errorf("keep refusing everything left hosts=%d models=%d providers=%d", len(none.Hosts), len(none.Models), len(none.Providers))
	}
}

// The document must show the rate sheet relay bills, which is the binding's explicit one ahead of the sheet covering the pair.
func TestFlatten_PricingFollowsTheBindingsSheet(t *testing.T) {
	idx, _ := flattenFixture()
	for _, h := range Flatten(idx, time.Time{}, nil).Hosts {
		if h.Name != "host-a" {
			continue
		}
		rates := h.Models[0].Pricing
		if len(rates) != 1 || rates[0].Amount != 7 {
			t.Fatalf("host-a rates = %+v, want the explicit sheet's 7", rates)
		}
		return
	}
	t.Fatal("host-a missing")
}
