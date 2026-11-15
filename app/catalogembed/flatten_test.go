package catalogembed

import (
	"github.com/google/go-cmp/cmp"
	sdkcatalog "github.com/wyolet/relay/sdk/catalog"
	"testing"
	"time"

	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/meta"
)

// flattenFixture is one model on two hosts. The first host owns two rate sheets for the pair; its binding names the one billing uses, which is not the one the (model, host) lookup returns.
func flattenFixture() (snap *catalog.Snapshot, keptBinding string) {
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
	snap = catalogtest.Catalog{
		Providers: []*provider.Provider{{Meta: meta.Metadata{ID: provID, Name: "prov", Owner: meta.Owner{Kind: meta.OwnerSystem}}}},
		Hosts: []*host.Host{
			{Meta: meta.Metadata{ID: hostA, Name: "host-a", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "http://a.example"}},
			{Meta: meta.Metadata{ID: hostB, Name: "host-b", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "http://b.example"}},
		},
		Models: []*model.Model{{
			Meta: meta.Metadata{ID: modID, Name: "m", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
			Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "m-snap"}}, Pointer: "m-snap"},
		}},
		// Both sheets target the pair; the later one wins the (model, host) lookup.
		Pricings: []*pricing.Pricing{sheet(explicitID, "explicit", 7), sheet(coverID, "cover", 1)},
		Bindings: []*binding.Binding{
			{Meta: meta.Metadata{ID: bindA, Name: "on-a", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: binding.Spec{ModelID: modID, HostID: hostA, Adapter: adapters.OpenAI, PricingID: explicitID}},
			{Meta: meta.Metadata{ID: meta.NewID(), Name: "on-b", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: binding.Spec{ModelID: modID, HostID: hostB, Adapter: adapters.OpenAI}},
		},
	}.Snapshot()
	return snap, bindA
}

// row is the document row of one served snapshot; amount 0 is unpriced.
func row(snapshot string, amount float64) sdkcatalog.Binding {
	b := sdkcatalog.Binding{Name: snapshot, MetadataName: snapshot, Adapter: "openai", Providers: []string{"prov"}}
	if amount != 0 {
		b.Pricing = []sdkcatalog.Rate{{Meter: "tokens.input", Unit: "per_million", Amount: amount}}
	}
	return b
}

func TestFlattenScoped_KeepFiltersBindings(t *testing.T) {
	snap, kept := flattenFixture()
	// host-a shows the binding's explicit sheet (7), not the later sheet covering the pair (1).
	hostA := sdkcatalog.Host{Name: "host-a", BaseURL: "http://a.example", Models: []sdkcatalog.Binding{row("m-snap", 7)}}
	hostB := sdkcatalog.Host{Name: "host-b", BaseURL: "http://b.example", Models: []sdkcatalog.Binding{row("m-snap", 0)}}
	doc := func(hosts ...sdkcatalog.Host) *sdkcatalog.Catalog {
		c := &sdkcatalog.Catalog{Version: "relay-catalog@v1alpha3", Hosts: hosts}
		if hosts != nil {
			c.Models = []sdkcatalog.ModelInfo{{MetadataName: "m-snap", Provider: "prov"}}
			c.Providers = []sdkcatalog.ProviderInfo{{Name: "prov"}}
		}
		return c
	}
	for _, c := range []struct {
		name string
		keep func(*model.Model, *binding.Binding) bool
		want *sdkcatalog.Catalog
	}{
		{"nil keeps all", nil, doc(hostA, hostB)},
		{"one binding", func(_ *model.Model, b *binding.Binding) bool { return b.Meta.ID == kept }, doc(hostA)},
		{"none", func(*model.Model, *binding.Binding) bool { return false }, doc()},
	} {
		t.Run(c.name, func(t *testing.T) {
			if diff := cmp.Diff(c.want, FlattenScoped(snap, time.Time{}, c.keep)); diff != "" {
				t.Fatalf("(-want +got):\n%s", diff)
			}
		})
	}
}
