package inference

import (
	"sort"
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/pkg/clientprofile"
)

// entriesFixture builds one model served by a keyed, priced host and a
// keyless (NoAuth) unpriced one, with an exact alias and a wildcard pattern
// alongside it. grants are the policy's modelref grants.
func entriesFixture(t *testing.T, grants ...string) (*catalog.Snapshot, *policy.Policy, []*model.Model) {
	t.Helper()

	provID, modID := meta.NewID(), meta.NewID()
	pricedHostID, freeHostID, priceID := meta.NewID(), meta.NewID(), meta.NewID()
	hkID, polID := meta.NewID(), meta.NewID()

	m := &model.Model{
		Meta: meta.Metadata{ID: modID, Name: "big-model", DisplayName: "Big Model", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
		Spec: model.Spec{
			Snapshots: []model.Snapshot{{Name: "big-model-2026"}, {Name: "big-model-2025"}},
			Pointer:   "big-model-2026",
			Aliases:   []string{"big", "big-model-2026[*]"},
		},
	}
	hosts := []*host.Host{
		{Meta: meta.Metadata{ID: pricedHostID, Name: "priced-host", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "http://x.example"}},
		{Meta: meta.Metadata{ID: freeHostID, Name: "free-host", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "http://y.example", NoAuth: true}},
	}
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: hkID, Name: "k", Owner: meta.Owner{Kind: meta.OwnerHost, ID: pricedHostID}},
		Spec: hostkey.Spec{HostID: pricedHostID, PolicyID: polID, Value: "sk-test", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}},
	}
	pol := &policy.Policy{
		Meta: meta.Metadata{ID: polID, Name: "p", Owner: meta.Owner{Kind: meta.OwnerHost, ID: pricedHostID}},
		Spec: policy.Spec{Models: grants, HostKeyIDs: []string{hkID}},
	}
	price := &pricing.Pricing{
		Meta: meta.Metadata{ID: priceID, Name: "big-model-rates", Owner: meta.Owner{Kind: meta.OwnerHost, ID: pricedHostID}},
		Spec: pricing.Spec{
			Currency:       "USD",
			TargetModelIDs: []string{modID},
			Rates: []pricing.Rate{
				{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: 3},
				{Meter: pricing.MeterTokensOutput, Unit: pricing.UnitPerMillion, Amount: 15},
				// A context tier must not displace the base rate.
				{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: 6, AboveTokens: 200000},
			},
		},
	}
	bindings := []*binding.Binding{
		{
			Meta: meta.Metadata{ID: meta.NewID(), Name: "b-priced", Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: binding.Spec{ModelID: modID, HostID: pricedHostID, Adapter: adapters.Anthropic, PricingID: priceID},
		},
		{
			Meta: meta.Metadata{ID: meta.NewID(), Name: "b-free", Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: binding.Spec{ModelID: modID, HostID: freeHostID, Adapter: adapters.OpenAI},
		},
	}
	snap := catalog.Build(
		[]*provider.Provider{{Meta: meta.Metadata{ID: provID, Name: "prov", Owner: meta.Owner{Kind: meta.OwnerSystem}}}},
		hosts,
		[]*policy.Policy{pol},
		nil,
		[]*model.Model{m},
		[]*hostkey.HostKey{hk},
		nil,
		[]*pricing.Pricing{price},
		bindings,
	)
	pol, ok := snap.Policy(polID)
	if !ok {
		t.Fatal("policy missing from snapshot")
	}
	return snap, pol, []*model.Model{m}
}

func TestModelEntries_SnapshotsAliasesAndHosts(t *testing.T) {
	snap, pol, models := entriesFixture(t, "prov/big-model")
	entries := modelEntries(snap, pol, models)

	if len(entries) != 2 {
		t.Fatalf("entries len = %d, want one per snapshot", len(entries))
	}
	if entries[0].ID != "big-model-2026" || entries[1].ID != "big-model-2025" {
		t.Fatalf("entry ids = %q, %q", entries[0].ID, entries[1].ID)
	}
	if entries[0].DisplayName != "Big Model" {
		t.Errorf("display name = %q", entries[0].DisplayName)
	}

	// Aliases resolve to the pointer snapshot, and wildcard patterns are
	// not enumerable.
	if len(entries[0].Aliases) != 1 || entries[0].Aliases[0] != "big" {
		t.Errorf("pointer aliases = %v, want [big]", entries[0].Aliases)
	}
	if len(entries[1].Aliases) != 0 {
		t.Errorf("non-pointer snapshot must carry no aliases, got %v", entries[1].Aliases)
	}

	hosts := entries[0].Hosts
	if len(hosts) != 2 {
		t.Fatalf("hosts len = %d, want 2", len(hosts))
	}
	byName := map[string]int{}
	for i, h := range hosts {
		byName[h.Name] = i
	}
	priced := hosts[byName["priced-host"]]
	if !priced.Priced || priced.InputUSDPerMtok != 3 || priced.OutputUSDPerMtok != 15 {
		t.Errorf("priced host = %+v, want base-tier 3/15", priced)
	}
	if free := hosts[byName["free-host"]]; free.Priced {
		t.Errorf("host without a pricing row must stay unpriced, got %+v", free)
	}
}

// The parent slug and the pointer bit are what a profile needs to tell one
// model's snapshots apart — they all carry the same display name.
func TestModelEntries_CarriesParentSlugAndPointer(t *testing.T) {
	snap, pol, models := entriesFixture(t, "prov/big-model")
	entries := modelEntries(snap, pol, models)

	if len(entries) != 2 {
		t.Fatalf("entries len = %d, want one per snapshot", len(entries))
	}
	for _, e := range entries {
		if e.Model != "big-model" {
			t.Errorf("entry %q model = %q, want big-model", e.ID, e.Model)
		}
	}
	if !entries[0].Pointer {
		t.Errorf("%q is the pointer snapshot", entries[0].ID)
	}
	if entries[1].Pointer {
		t.Errorf("%q is not the pointer snapshot", entries[1].ID)
	}
}

// Every snapshot of a model inherits the model's catalog metadata: a
// profile sizes a session and labels capabilities off these.
func TestModelEntries_CarriesCatalogMetadata(t *testing.T) {
	snap, pol, models := entriesFixture(t, "prov/big-model")
	models[0].Spec.ContextWindowTotal = 1000000
	models[0].Spec.MaxOutputTokens = 384000
	models[0].Spec.Capabilities = model.Capabilities{Reasoning: true, Tools: true}

	entries := modelEntries(snap, pol, models)
	if len(entries) != 2 {
		t.Fatalf("entries len = %d, want one per snapshot", len(entries))
	}
	for _, e := range entries {
		if e.ContextWindow != 1000000 {
			t.Errorf("%q context window = %d", e.ID, e.ContextWindow)
		}
		if e.MaxOutputTokens != 384000 {
			t.Errorf("%q max output tokens = %d", e.ID, e.MaxOutputTokens)
		}
		if !e.Reasoning || !e.ToolCall {
			t.Errorf("%q capabilities = reasoning:%v toolCall:%v", e.ID, e.Reasoning, e.ToolCall)
		}
	}
}

// A model publishing only the input/output split still gets a window; a
// model declaring none gets a zero, never a substituted number.
func TestModelEntries_ContextWindowFallsBackToInput(t *testing.T) {
	snap, pol, models := entriesFixture(t, "prov/big-model")
	models[0].Spec.ContextWindowInput = 272000

	entries := modelEntries(snap, pol, models)
	if len(entries) == 0 {
		t.Fatal("no entries")
	}
	if entries[0].ContextWindow != 272000 {
		t.Errorf("context window = %d, want the input window", entries[0].ContextWindow)
	}

	snap, pol, models = entriesFixture(t, "prov/big-model")
	entries = modelEntries(snap, pol, models)
	if entries[0].ContextWindow != 0 || entries[0].MaxOutputTokens != 0 {
		t.Errorf("undeclared metadata = %d/%d, want zeros", entries[0].ContextWindow, entries[0].MaxOutputTokens)
	}
}

// A document that prints prices needs the cache meters and the context
// tiers, each read at its own threshold rather than patched onto the base.
func TestModelEntries_CarriesCacheRatesAndTiers(t *testing.T) {
	snap, pol, models := entriesFixture(t, "prov/big-model")
	for _, p := range snap.AllPricings() {
		p.Spec.Rates = append(p.Spec.Rates,
			pricing.Rate{Meter: pricing.MeterTokensCacheRead, Unit: pricing.UnitPerMillion, Amount: 0.3},
			pricing.Rate{Meter: pricing.MeterTokensCacheCreation, Unit: pricing.UnitPerMillion, Amount: 3.75},
		)
	}

	entries := modelEntries(snap, pol, models)
	var priced clientprofile.ModelHost
	for _, h := range entries[0].Hosts {
		if h.Name == "priced-host" {
			priced = h
		}
	}
	if priced.CacheReadUSDPerMtok == nil || *priced.CacheReadUSDPerMtok != 0.3 {
		t.Errorf("cache read = %v", priced.CacheReadUSDPerMtok)
	}
	if priced.CacheWriteUSDPerMtok == nil || *priced.CacheWriteUSDPerMtok != 3.75 {
		t.Errorf("cache write = %v", priced.CacheWriteUSDPerMtok)
	}
	if len(priced.Tiers) != 1 {
		t.Fatalf("tiers = %+v, want the fixture's single context tier", priced.Tiers)
	}
	tier := priced.Tiers[0]
	// The sheet tiers input alone; the tier still carries every meter's
	// price at that point, the untiered ones at their base rate.
	if tier.AboveTokens != 200000 || tier.InputUSDPerMtok != 6 || tier.OutputUSDPerMtok != 15 {
		t.Errorf("tier = %+v", tier)
	}
	if tier.CacheReadUSDPerMtok == nil || *tier.CacheReadUSDPerMtok != 0.3 {
		t.Errorf("tier cache read = %v", tier.CacheReadUSDPerMtok)
	}
}

// A meter the sheet does not price stays absent: nil is "unpriced", which
// a projection must not print as free.
func TestModelEntries_UnpricedCacheMetersStayNil(t *testing.T) {
	snap, pol, models := entriesFixture(t, "prov/big-model")
	for _, h := range modelEntries(snap, pol, models)[0].Hosts {
		if h.CacheReadUSDPerMtok != nil || h.CacheWriteUSDPerMtok != nil {
			t.Errorf("%q cache rates = %v / %v, want nil", h.Name, h.CacheReadUSDPerMtok, h.CacheWriteUSDPerMtok)
		}
	}
}

// Modalities, release date and the temperature bit come off the catalog as
// stated; temperature is the inverse of the parameters the model rejects.
func TestModelEntries_CarriesModalitiesReleaseDateAndTemperature(t *testing.T) {
	snap, pol, models := entriesFixture(t, "prov/big-model")
	models[0].Spec.Modalities = model.Modalities{Input: []string{"text", "image"}, Output: []string{"text"}}
	models[0].Spec.Capabilities = model.Capabilities{Vision: true, UnsupportedParams: []string{"temperature"}}
	models[0].Spec.Snapshots[0].ReleasedAt = "2026-02-17"

	entries := modelEntries(snap, pol, models)
	if got := entries[0].ReleasedAt; got != "2026-02-17" {
		t.Errorf("released at = %q", got)
	}
	if got := entries[1].ReleasedAt; got != "" {
		t.Errorf("undeclared release date = %q, want empty", got)
	}
	for _, e := range entries {
		if len(e.Modalities.Input) != 2 || e.Modalities.Input[1] != "image" {
			t.Errorf("%q modalities = %+v", e.ID, e.Modalities)
		}
		if !e.Vision {
			t.Errorf("%q must carry the vision capability", e.ID)
		}
		if e.Temperature {
			t.Errorf("%q rejects temperature, so the entry must not offer it", e.ID)
		}
	}
}

// restrictBinding pins one binding to the named snapshots, the catalog's
// way of saying a host serves only part of a model's snapshot set.
func restrictBinding(t *testing.T, snap *catalog.Snapshot, bindingName string, snapshots ...string) {
	t.Helper()
	for _, b := range snap.AllBindings() {
		if b.Meta.Name == bindingName {
			b.Spec.Snapshots = snapshots
			return
		}
	}
	t.Fatalf("binding %q not in snapshot", bindingName)
}

func hostNames(hosts []clientprofile.ModelHost) []string {
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, h.Name)
	}
	sort.Strings(out)
	return out
}

// A binding restricted to one snapshot must not advertise the others — the
// same Serves gate routing applies when it picks a binding.
func TestModelEntries_HostsFollowTheBindingSnapshotSet(t *testing.T) {
	snap, pol, models := entriesFixture(t, "prov/big-model")
	restrictBinding(t, snap, "b-priced", "big-model-2026")

	entries := modelEntries(snap, pol, models)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want both snapshots served", len(entries))
	}
	if got := hostNames(entries[0].Hosts); len(got) != 2 {
		t.Errorf("%q hosts = %v, want both", entries[0].ID, got)
	}
	if got := hostNames(entries[1].Hosts); len(got) != 1 || got[0] != "free-host" {
		t.Errorf("%q hosts = %v, want [free-host]", entries[1].ID, got)
	}
}

// With the only granted host restricted to one snapshot, the others are not
// addressable at all and must not be listed.
func TestModelEntries_SkipsSnapshotsNoGrantedBindingServes(t *testing.T) {
	snap, pol, models := entriesFixture(t, "prov/big-model@priced-host")
	restrictBinding(t, snap, "b-priced", "big-model-2026")

	entries := modelEntries(snap, pol, models)
	if len(entries) != 1 || entries[0].ID != "big-model-2026" {
		t.Fatalf("entries = %+v, want big-model-2026 alone", entries)
	}
	if got := hostNames(entries[0].Hosts); len(got) != 1 || got[0] != "priced-host" {
		t.Errorf("hosts = %v, want [priced-host]", got)
	}
}

// The OpenAI-shaped listings share the rule: an unserved snapshot is not a
// model id anyone can send.
func TestAppendModelRows_SkipsSnapshotsNoGrantedBindingServes(t *testing.T) {
	snap, pol, models := entriesFixture(t, "prov/big-model@priced-host")
	restrictBinding(t, snap, "b-priced", "big-model-2026")

	var rows []modelObject
	seen := map[string]struct{}{}
	m := models[0]
	appendModelRows(&rows, snap, m, grantedBindings(snap, pol, m, ""), seen)
	if len(rows) != 1 || rows[0].ID != "big-model-2026" {
		t.Fatalf("rows = %+v, want big-model-2026 alone", rows)
	}
}

func TestModelEntries_SkipsDisabledBindings(t *testing.T) {
	snap, pol, models := entriesFixture(t, "prov/big-model")
	disabled := false
	for _, b := range snap.AllBindings() {
		b.Spec.Enabled = &disabled
	}
	for _, e := range modelEntries(snap, pol, models) {
		if len(e.Hosts) != 0 {
			t.Fatalf("disabled bindings must not surface a host: %+v", e.Hosts)
		}
	}
}

// A host outside the grant must not be advertised — the description would
// otherwise name a route the caller's key cannot take.
func TestModelEntries_ListsOnlyGrantedHosts(t *testing.T) {
	snap, pol, models := entriesFixture(t, "prov/big-model@priced-host")
	entries := modelEntries(snap, pol, models)
	if len(entries) == 0 {
		t.Fatal("no entries")
	}
	hosts := entries[0].Hosts
	if len(hosts) != 1 || hosts[0].Name != "priced-host" {
		t.Fatalf("hosts = %+v, want priced-host alone", hosts)
	}
}
