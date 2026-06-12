package routing_test

import (
	"errors"
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/relaykey"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/pkg/slug"
)

// aliasCatalog: two models on one host. m1 ("gpt-5-5", granted by the
// policy) declares an exact bracket alias + a wildcard; m2
// ("secret-model", NOT granted) declares its own alias — proving the
// policy gate runs on the resolved model, not on how it was addressed.
func aliasCatalog(t *testing.T) (*catalog.Catalog, *relaykey.RelayKey) {
	t.Helper()
	provID, hostID := meta.NewID(), meta.NewID()
	hkID, m1ID, m2ID, polID := meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID()

	prov := &provider.Provider{Meta: meta.Metadata{ID: provID, Name: "openai", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	h := &host.Host{Meta: meta.Metadata{ID: hostID, Name: "openai", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "http://up.example"}}
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: hkID, Name: "k", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}},
		Spec: hostkey.Spec{HostID: hostID, PolicyID: polID, Value: "sk", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}},
	}
	m1 := &model.Model{
		Meta: meta.Metadata{ID: m1ID, Name: "gpt-5-5", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
		Spec: model.Spec{
			Snapshots: []model.Snapshot{mkSnap("gpt-5.5"), mkSnap("gpt-5.5-2026-04-23")},
			Pointer:   slug.From("gpt-5.5"),
			Aliases:   []string{"gpt-5-5[1m]", "gpt-5-5x[*]"},
		},
	}
	m2 := &model.Model{
		Meta: meta.Metadata{ID: m2ID, Name: "secret-model", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
		Spec: model.Spec{
			Snapshots: []model.Snapshot{mkSnap("secret-model")},
			Pointer:   "secret-model",
			Aliases:   []string{"secret[1m]"},
		},
	}
	b1 := &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "m1-on-openai", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: m1ID, HostID: hostID, Adapter: adapters.OpenAI},
	}
	b2 := &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "m2-on-openai", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: m2ID, HostID: hostID, Adapter: adapters.OpenAI},
	}
	pol := &policy.Policy{
		Meta: meta.Metadata{ID: polID, Name: "p", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}},
		Spec: policy.Spec{ModelIDs: []string{m1ID}, HostKeyIDs: []string{hkID}},
	}
	rk := &relaykey.RelayKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "rk", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: relaykey.Spec{PolicyID: polID, KeyHash: "h"},
	}

	c := catalog.New(provListR{prov}, hostListR{h}, polListR{pol}, modListR{m1, m2},
		keyListR{hk}, rlListR{}, rkListR{rk}, rcListR{}, bndListR{b1, b2})
	if err := c.Reload(t.Context()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	return c, rk
}

func TestResolve_AliasExactVerbatim(t *testing.T) {
	c, rk := aliasCatalog(t)
	r := routing.New(c)

	for _, send := range []string{
		"gpt-5-5[1m]", // as declared
		"gpt-5.5[1M]", // normalization variant — still the exact alias
		"GPT-5-5.1m",
	} {
		t.Run(send, func(t *testing.T) {
			plan, err := r.Resolve(routing.Request{ModelName: send, RawModelName: send, RelayKey: rk})
			if err != nil {
				t.Fatalf("Resolve(%q): %v", send, err)
			}
			// Exact alias → the DECLARED string goes upstream, not the
			// caller's spelling and not the snapshot's upstream name.
			if got := plan.UpstreamModel(); got != "gpt-5-5[1m]" {
				t.Errorf("UpstreamModel() = %q, want declared alias", got)
			}
			if plan.ResolvedVia != "alias:gpt-5-5[1m]" {
				t.Errorf("ResolvedVia = %q", plan.ResolvedVia)
			}
			// Identity is the real model + pointer snapshot.
			if plan.Model.Meta.Name != "gpt-5-5" || plan.Snapshot.Name != slug.From("gpt-5.5") {
				t.Errorf("identity: model=%q snapshot=%q", plan.Model.Meta.Name, plan.Snapshot.Name)
			}
		})
	}
}

func TestResolve_AliasPatternVerbatim(t *testing.T) {
	c, rk := aliasCatalog(t)
	r := routing.New(c)

	raw := "gpt-5-5x[experimental-2027]"
	plan, err := r.Resolve(routing.Request{ModelName: raw, RawModelName: raw, RelayKey: rk})
	if err != nil {
		t.Fatalf("Resolve(%q): %v", raw, err)
	}
	// Wildcard → the caller's RAW string goes upstream verbatim.
	if got := plan.UpstreamModel(); got != raw {
		t.Errorf("UpstreamModel() = %q, want %q", got, raw)
	}
	if plan.ResolvedVia != "alias:gpt-5-5x[*]" {
		t.Errorf("ResolvedVia = %q", plan.ResolvedVia)
	}
	if plan.Model.Meta.Name != "gpt-5-5" {
		t.Errorf("model = %q", plan.Model.Meta.Name)
	}

	// RawModelName empty → falls back to ModelName.
	plan, err = r.Resolve(routing.Request{ModelName: raw, RelayKey: rk})
	if err != nil {
		t.Fatalf("Resolve no-raw: %v", err)
	}
	if got := plan.UpstreamModel(); got != raw {
		t.Errorf("fallback UpstreamModel() = %q, want %q", got, raw)
	}
}

func TestResolve_AliasPinnedAndPatternPinSkip(t *testing.T) {
	c, rk := aliasCatalog(t)
	r := routing.New(c)

	// Exact alias with a host pin resolves via the synthesized pinned form
	// and still carries the clean declared alias upstream (no "@host").
	plan, err := r.Resolve(routing.Request{ModelName: "gpt-5-5[1m]@openai", RawModelName: "gpt-5-5[1m]@openai", RelayKey: rk})
	if err != nil {
		t.Fatalf("pinned exact alias: %v", err)
	}
	if got := plan.UpstreamModel(); got != "gpt-5-5[1m]" {
		t.Errorf("pinned exact: UpstreamModel() = %q, want declared alias", got)
	}
	if plan.Host.Meta.Name != "openai" {
		t.Errorf("pinned exact: host = %q", plan.Host.Meta.Name)
	}

	// A pinned ref never matches a wildcard (the pin segment would corrupt
	// the match) — it 404s instead of routing with a mangled wire name.
	_, err = r.Resolve(routing.Request{ModelName: "gpt-5-5x[thing]@openai", RawModelName: "gpt-5-5x[thing]@openai", RelayKey: rk})
	if !errors.Is(err, routing.ErrModelNotFound) {
		t.Fatalf("pinned pattern ref: err = %v, want ErrModelNotFound", err)
	}
}

func TestResolve_AliasPolicyGateStillApplies(t *testing.T) {
	c, rk := aliasCatalog(t)
	r := routing.New(c)

	// m2's alias resolves the model, but the policy doesn't grant m2 —
	// addressing via alias must not widen the grant.
	_, err := r.Resolve(routing.Request{ModelName: "secret[1m]", RawModelName: "secret[1m]", RelayKey: rk})
	if !errors.Is(err, routing.ErrModelNotInPolicy) {
		t.Fatalf("alias to ungranted model: err = %v, want ErrModelNotInPolicy", err)
	}
}

func TestResolve_NoAliasNoOverride(t *testing.T) {
	c, rk := aliasCatalog(t)
	r := routing.New(c)

	plan, err := r.Resolve(routing.Request{ModelName: "gpt-5.5", RawModelName: "gpt-5.5", RelayKey: rk})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plan.UpstreamOverride != "" || plan.ResolvedVia != "" {
		t.Errorf("normal resolution must not set alias fields: override=%q via=%q",
			plan.UpstreamOverride, plan.ResolvedVia)
	}
	if got := plan.UpstreamModel(); got != "gpt-5.5" {
		t.Errorf("UpstreamModel() = %q, want snapshot upstream", got)
	}
}

func TestResolve_RealNameShadowsAlias(t *testing.T) {
	// m1 declares an alias that normalizes exactly to m1's OTHER (dated)
	// snapshot name on a fresh catalog — Validate forbids self-shadowing,
	// so use m2 shadowing m1's snapshot instead.
	c, rk := aliasCatalog(t)
	snap := c.Current()
	m2 := *snap.ModelsByName("secret-model")[0]
	m2.Spec.Aliases = append(m2.Spec.Aliases, "gpt-5.5-2026.04.23") // normalizes to m1's dated snapshot
	if err := c.ApplyModelUpsert(&m2); err != nil {
		t.Fatal(err)
	}
	plan, err := routing.New(c).Resolve(routing.Request{ModelName: "gpt-5.5-2026-04-23", RawModelName: "gpt-5.5-2026-04-23", RelayKey: rk})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plan.Model.Meta.Name != "gpt-5-5" || plan.ResolvedVia != "" {
		t.Errorf("real snapshot name must win over m2's alias: model=%q via=%q",
			plan.Model.Meta.Name, plan.ResolvedVia)
	}
}
