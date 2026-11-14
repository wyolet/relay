package catalogembed

import (
	"github.com/google/go-cmp/cmp"
	sdkcatalog "github.com/wyolet/relay/sdk/catalog"
	"testing"
	"time"

	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/embed"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/meta"
)

// The catalog tools flatten plain rows through embed.Index; relay flattens its snapshot. Over rows the snapshot build drops or rewrites, both must produce the same document.
func TestFlatten_IndexMatchesSnapshot(t *testing.T) {
	off := func() *bool { f := false; return &f }
	sys := meta.Owner{Kind: meta.OwnerSystem}
	provID, goneProvID := meta.NewID(), meta.NewID()
	hostA, hostB, hostOff, goneHost := meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID()
	mod, modOff, orphan, goneModel := meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID()
	explicit, cover, goneHostSheet, noTargetSheet := meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID()

	provs := []*provider.Provider{
		{Meta: meta.Metadata{ID: provID, Name: "prov", DisplayName: "Prov", Owner: sys}},
		{Meta: meta.Metadata{ID: meta.NewID(), Name: "unused", Owner: sys}},
	}
	hosts := []*host.Host{
		{Meta: meta.Metadata{ID: hostA, Name: "host-a", Owner: sys}, Spec: host.Spec{BaseURL: "http://a.example"}},
		{Meta: meta.Metadata{ID: hostB, Name: "host-b", Owner: sys}, Spec: host.Spec{BaseURL: "http://b.example"}},
		{Meta: meta.Metadata{ID: hostOff, Name: "host-off", Owner: sys}, Spec: host.Spec{BaseURL: "http://off.example", Enabled: off()}},
	}
	snaps := func(names ...string) []model.Snapshot {
		out := make([]model.Snapshot, len(names))
		for i, n := range names {
			out[i] = model.Snapshot{Name: n}
		}
		return out
	}
	models := []*model.Model{
		{Meta: meta.Metadata{ID: mod, Name: "m", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}, Labels: map[string]string{"featured": "true"}},
			Spec: model.Spec{Snapshots: snaps("m-1", "m-2"), Pointer: "m-2", Aliases: []string{"m-latest"}}},
		{Meta: meta.Metadata{ID: modOff, Name: "m-off", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
			Spec: model.Spec{Snapshots: snaps("off-1"), Pointer: "off-1", Enabled: off()}},
		// Its provider is not in the rows, so the snapshot drops it.
		{Meta: meta.Metadata{ID: orphan, Name: "orphan", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: goneProvID}},
			Spec: model.Spec{Snapshots: snaps("orphan-1"), Pointer: "orphan-1"}},
	}
	sheet := func(id, owner string, amount float64, enabled *bool, targets ...string) *pricing.Pricing {
		return &pricing.Pricing{
			Meta: meta.Metadata{ID: id, Name: "sheet-" + id, Owner: meta.Owner{Kind: meta.OwnerHost, ID: owner}},
			Spec: pricing.Spec{Currency: "USD", TargetModelIDs: targets, Enabled: enabled,
				Rates: []pricing.Rate{{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: amount}}},
		}
	}
	pricings := []*pricing.Pricing{
		sheet(explicit, hostA, 7, nil, mod),
		sheet(cover, hostA, 1, nil, mod, goneModel),
		sheet(goneHostSheet, goneHost, 3, nil, mod),
		sheet(noTargetSheet, hostA, 4, nil, orphan, goneModel),
		// The disabled sheet covers host-b's pair last, so that binding is unpriced.
		sheet(meta.NewID(), hostB, 2, nil, mod),
		sheet(meta.NewID(), hostB, 5, off(), mod),
		sheet(meta.NewID(), hostOff, 6, nil, mod),
	}
	bind := func(name, modelID, hostID, pricingID string, enabled *bool, served ...string) *binding.Binding {
		return &binding.Binding{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: sys},
			Spec: binding.Spec{ModelID: modelID, HostID: hostID, Adapter: adapters.OpenAI, PricingID: pricingID, Enabled: enabled, Snapshots: served},
		}
	}
	bindings := []*binding.Binding{
		bind("z-explicit", mod, hostA, explicit, nil),
		bind("a-gone-sheet", mod, hostA, goneHostSheet, nil, "m-1"),
		bind("b-no-target-sheet", mod, hostA, noTargetSheet, nil, "m-2"),
		bind("on-b", mod, hostB, "", nil),
		bind("on-off-host", mod, hostOff, "", nil),
		bind("disabled", modOff, hostA, "", off()),
		bind("orphan-a", orphan, hostA, "", nil),
		bind("gone-model", goneModel, hostA, "", nil),
		bind("gone-host", mod, goneHost, "", nil),
	}

	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fromSnap, err := embed.MarshalJSON(FlattenScoped(catalog.Build(provs, hosts, nil, nil, models, nil, nil, pricings, bindings), at, nil))
	if err != nil {
		t.Fatal(err)
	}
	cat := embed.Flatten(embed.NewIndex(provs, hosts, models, pricings, bindings), at, nil)
	fromIndex, err := embed.MarshalJSON(cat)
	if err != nil {
		t.Fatal(err)
	}
	if string(fromSnap) != string(fromIndex) {
		t.Fatalf("index flatten differs from snapshot flatten:\nsnapshot %s\nindex    %s", fromSnap, fromIndex)
	}
	// The fixture must exercise the drops, or agreement proves little.
	pointer := func(amount float64) sdkcatalog.Binding {
		b := row("m-2", amount)
		b.Featured, b.Aliases = true, []string{"m-latest"}
		return b
	}
	want := &sdkcatalog.Catalog{
		Version:     "relay-catalog@v1alpha3",
		GeneratedAt: at,
		Hosts: []sdkcatalog.Host{
			// A binding whose own sheet is gone or doesn't target it falls back to the covering sheet (1).
			{Name: "host-a", BaseURL: "http://a.example", Models: []sdkcatalog.Binding{row("m-1", 1), row("m-1", 7), pointer(1), pointer(7)}},
			{Name: "host-b", BaseURL: "http://b.example", Models: []sdkcatalog.Binding{row("m-1", 0), pointer(0)}},
		},
		Models:    []sdkcatalog.ModelInfo{{MetadataName: "m-1", Provider: "prov"}, {MetadataName: "m-2", Provider: "prov"}},
		Providers: []sdkcatalog.ProviderInfo{{Name: "prov", DisplayName: "Prov"}},
	}
	if diff := cmp.Diff(want, cat); diff != "" {
		t.Fatalf("document (-want +got):\n%s", diff)
	}
}
