package inference

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/clientprofile"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

func perMillion(m pricing.Meter, amount float64, above int, tier string) pricing.Rate {
	return pricing.Rate{Meter: m, Unit: pricing.UnitPerMillion, Amount: amount, AboveTokens: above, ServiceTier: tier}
}

// entriesFixture is big-model, two snapshots, bound to priced-host (a tiered sheet with cache meters and service-tier rows), unit-host (priced per token), half-priced-host (an input rate alone) and free-host (no sheet). edit changes the model before the snapshot is built.
func entriesFixture(edit func(*model.Model)) (*catalog.Snapshot, *model.Model) {
	provID, modID := meta.NewID(), meta.NewID()
	system := meta.Owner{Kind: meta.OwnerSystem}
	m := &model.Model{
		Meta: meta.Metadata{ID: modID, Name: "big-model", DisplayName: "Big Model", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
		Spec: model.Spec{
			Snapshots:          []model.Snapshot{{Name: "big-model-2026", ReleasedAt: "2026-02-17"}, {Name: "big-model-2025", ReleasedAt: "yesterday"}},
			Pointer:            "Big-Model-2026",
			Aliases:            []string{"big", "big-model-2026[*]"},
			ContextWindowTotal: 1000000,
			ContextWindowInput: 272000,
			MaxOutputTokens:    384000,
			Capabilities:       model.Capabilities{Reasoning: true, Tools: true, Vision: true, UnsupportedParams: []string{"top_p", "temperature"}},
			Modalities:         model.Modalities{Input: []string{"text", "image"}, Output: []string{"text"}},
		},
	}
	if edit != nil {
		edit(m)
	}
	c := catalogtest.Catalog{
		Providers: []*provider.Provider{{Meta: meta.Metadata{ID: provID, Name: "prov", Owner: system}}},
		Models:    []*model.Model{m},
	}
	for _, h := range []struct {
		name  string
		rates []pricing.Rate
	}{
		{"priced-host", []pricing.Rate{
			perMillion(pricing.MeterTokensInput, 3, 0, ""),
			perMillion(pricing.MeterTokensOutput, 15, 0, ""),
			perMillion(pricing.MeterTokensCacheRead, 0.3, 0, ""),
			perMillion(pricing.MeterTokensCacheCreation, 3.75, 0, ""),
			perMillion(pricing.MeterTokensInput, 12, 400000, ""),
			perMillion(pricing.MeterTokensInput, 6, 200000, ""),
			perMillion(pricing.MeterTokensCacheRead, 0.6, 200000, ""),
			perMillion(pricing.MeterTokensInput, 5, 0, "priority"),
			perMillion(pricing.MeterTokensInput, 20, 800000, "priority"),
		}},
		{"unit-host", []pricing.Rate{
			{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerUnit, Amount: 0.5},
			{Meter: pricing.MeterTokensOutput, Unit: pricing.UnitPerUnit, Amount: 0.25},
		}},
		{"half-priced-host", []pricing.Rate{perMillion(pricing.MeterTokensInput, 3, 0, "")}},
		{"free-host", nil},
	} {
		hostID := meta.NewID()
		c.Hosts = append(c.Hosts, &host.Host{Meta: meta.Metadata{ID: hostID, Name: h.name, Owner: system}, Spec: host.Spec{BaseURL: "http://" + h.name + ".example"}})
		b := &binding.Binding{Meta: meta.Metadata{ID: meta.NewID(), Name: "at-" + h.name, Owner: system}, Spec: binding.Spec{ModelID: modID, HostID: hostID, Adapter: adapters.Anthropic}}
		if h.rates != nil {
			p := &pricing.Pricing{
				Meta: meta.Metadata{ID: meta.NewID(), Name: h.name + "-rates", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}},
				Spec: pricing.Spec{Currency: "USD", TargetModelIDs: []string{modID}, Rates: h.rates},
			}
			c.Pricings = append(c.Pricings, p)
			b.Spec.PricingID = p.Meta.ID
		}
		c.Bindings = append(c.Bindings, b)
	}
	return c.Snapshot(), m
}

// listedAt is what a route table lists for m when the caller reaches it at the named hosts, in that order.
func listedAt(t *testing.T, snap *catalog.Snapshot, m *model.Model, hosts ...string) []plan.Listed {
	t.Helper()
	l := plan.Listed{Model: m}
	for _, name := range hosts {
		found := false
		for _, b := range snap.BindingsForModel(m.Meta.ID) {
			if slug, _ := snap.HostSlug(b.Spec.HostID); slug == name {
				l.Bindings, found = append(l.Bindings, b), true
			}
		}
		if !found {
			t.Fatalf("no binding at host %q", name)
		}
	}
	return []plan.Listed{l}
}

func TestModelEntries(t *testing.T) {
	snap, m := entriesFixture(nil)
	f := func(v float64) *float64 { return &v }
	hosts := []clientprofile.ModelHost{
		{
			Name: "priced-host", Priced: true, InputUSDPerMtok: 3, OutputUSDPerMtok: 15, CacheReadUSDPerMtok: f(0.3), CacheWriteUSDPerMtok: f(3.75),
			// Each tier carries every meter at that point, the untiered ones at their base rate; service-tier rows add none.
			Tiers: []clientprofile.PriceTier{
				{AboveTokens: 200000, InputUSDPerMtok: 6, OutputUSDPerMtok: 15, CacheReadUSDPerMtok: f(0.6), CacheWriteUSDPerMtok: f(3.75)},
				{AboveTokens: 400000, InputUSDPerMtok: 12, OutputUSDPerMtok: 15, CacheReadUSDPerMtok: f(0.6), CacheWriteUSDPerMtok: f(3.75)},
			},
		},
		{Name: "unit-host", Priced: true, InputUSDPerMtok: 500000, OutputUSDPerMtok: 250000},
		// A host counts as priced only with a rate for both sides of a request.
		{Name: "half-priced-host"},
		{Name: "free-host"},
	}
	entry := clientprofile.ModelEntry{
		Model: "big-model", DisplayName: "Big Model", Hosts: hosts,
		ContextWindow: 1000000, MaxOutputTokens: 384000, Reasoning: true, ToolCall: true, Vision: true,
		Modalities: clientprofile.Modalities{Input: []string{"text", "image"}, Output: []string{"text"}},
	}
	latest, older := entry, entry
	// Aliases ride the pointer snapshot; a wildcard pattern cannot be listed.
	latest.ID, latest.Pointer, latest.Aliases, latest.ReleasedAt = "big-model-2026", true, []string{"big"}, "2026-02-17"
	older.ID, older.ReleasedAt = "big-model-2025", "yesterday"

	got := modelEntries(snap, listedAt(t, snap, m, "priced-host", "unit-host", "half-priced-host", "free-host"))
	if diff := cmp.Diff([]clientprofile.ModelEntry{latest, older}, got); diff != "" {
		t.Errorf("entries (-want +got):\n%s", diff)
	}
}

// A model that declares nothing lists zeros, never a substituted number, and its snapshot name stands in for the display name.
func TestModelEntries_UndeclaredMetadata(t *testing.T) {
	snap, m := entriesFixture(func(m *model.Model) {
		m.Meta.DisplayName = ""
		m.Spec = model.Spec{Snapshots: []model.Snapshot{{Name: "bare-1"}}, ContextWindowInput: 272000}
	})
	want := []clientprofile.ModelEntry{{ID: "bare-1", Model: "big-model", DisplayName: "bare-1", Hosts: []clientprofile.ModelHost{{Name: "free-host"}}, ContextWindow: 272000, Temperature: true}}
	if diff := cmp.Diff(want, modelEntries(snap, listedAt(t, snap, m, "free-host"))); diff != "" {
		t.Errorf("entries (-want +got):\n%s", diff)
	}
}

// A snapshot is listed with the hosts whose binding serves it, and not at all when none does: the gate routing applies when it picks a binding.
func TestModelListings_FollowTheBindingSnapshotSet(t *testing.T) {
	snap, m := entriesFixture(nil)
	for _, b := range snap.BindingsForModel(m.Meta.ID) {
		if b.Meta.Name == "at-priced-host" {
			b.Spec.Snapshots = []string{"big-model-2026"}
		}
	}
	both := []string{"big-model-2026", "big-model-2025"}
	free := listedAt(t, snap, m, "free-host")
	for _, tc := range []struct {
		name    string
		listing []plan.Listed
		ids     []string
		hosts   map[string][]string // snapshot name → hosts
	}{
		{"restricted host and an unrestricted one", listedAt(t, snap, m, "priced-host", "free-host"), both,
			map[string][]string{"big-model-2026": {"priced-host", "free-host"}, "big-model-2025": {"free-host"}}},
		{"restricted host alone", listedAt(t, snap, m, "priced-host"), both[:1], map[string][]string{"big-model-2026": {"priced-host"}}},
		{"a snapshot name listed twice keeps its first entry", append(free, free...), both,
			map[string][]string{"big-model-2026": {"free-host"}, "big-model-2025": {"free-host"}}},
		{"no bindings", []plan.Listed{{Model: m}}, nil, map[string][]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hosts := map[string][]string{}
			var entryIDs []string
			for _, e := range modelEntries(snap, tc.listing) {
				entryIDs = append(entryIDs, e.ID)
				for _, h := range e.Hosts {
					hosts[e.ID] = append(hosts[e.ID], h.Name)
				}
			}
			if diff := cmp.Diff(tc.ids, entryIDs); diff != "" {
				t.Errorf("entry ids (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.hosts, hosts); diff != "" {
				t.Errorf("hosts by snapshot (-want +got):\n%s", diff)
			}

			var rows, want []modelObject
			seen := map[string]struct{}{}
			for _, l := range tc.listing {
				appendModelRows(&rows, snap, l.Model, l.Bindings, seen)
			}
			for _, id := range tc.ids {
				// A release date that does not parse falls back to when the model was added.
				created := ids.UnixSec(m.Meta.ID)
				if id == "big-model-2026" {
					created = time.Date(2026, 2, 17, 0, 0, 0, 0, time.UTC).Unix()
				}
				want = append(want, modelObject{ID: id, Object: "model", Created: created, OwnedBy: "prov"})
			}
			if diff := cmp.Diff(want, rows); diff != "" {
				t.Errorf("list-models rows (-want +got):\n%s", diff)
			}
		})
	}
}

// /v1/models never advertises a model a request would be refused: a model outside the grant, and one granted only at a keyless host no pool reaches, are both refused by the route and both absent from the list. The /openai listing keeps only models bound in that shape.
func TestListModels(t *testing.T) {
	_, p := listingWorld(t)
	ctx := context.WithValue(context.Background(), ctxPrincipalT{}, p)
	allowed := p.Caller.Routes.Catalog().ModelsByName("allowed")[0]
	row := modelObject{ID: "allowed", Object: "model", Created: ids.UnixSec(allowed.Meta.ID), OwnedBy: "prov"}
	for filter, want := range map[adapters.Name][]modelObject{"": {row}, adapters.OpenAI: {row}, adapters.Anthropic: nil} {
		out, err := listModels(ctx, filter)
		if err != nil {
			t.Fatalf("filter %q: %v", filter, err)
		}
		if diff := cmp.Diff(want, out.Body.Data); diff != "" || out.Body.Object != "list" {
			t.Errorf("filter %q: object %q, rows (-want +got):\n%s", filter, out.Body.Object, diff)
		}
	}
	for name, routed := range map[string]bool{"allowed": true, "denied": false, "keyless": false} {
		if r := p.Caller.Routes.Get(name); (r.Refused == nil) != routed {
			t.Errorf("route %s refused = %v, want routed = %v", name, r.Refused, routed)
		}
	}

	var status huma.StatusError
	if _, err := listModels(context.Background(), ""); !errors.As(err, &status) || status.GetStatus() != http.StatusUnauthorized {
		t.Errorf("no principal: err = %v, want a 401", err)
	}
}

func TestListModels_NoCredentialCarriesTheCode(t *testing.T) {
	t.Skip(`bug: a listing reached with no principal (anonymous proxy mode) → 401 {"type":"invalid_request_error","message":"missing relay key"} with no code; the credential middleware answers the same condition with code "unauthenticated"`)
	httpapi.InstallErrorRewriter()
	_, err := listModels(context.Background(), "")
	var refused *httpapi.APIError
	if !errors.As(err, &refused) || refused.Err.Code != "unauthenticated" {
		t.Fatalf("err = %v, want the unauthenticated envelope", err)
	}
}
