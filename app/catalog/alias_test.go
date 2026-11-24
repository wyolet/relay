package catalog

import (
	"testing"

	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/pkg/slug"
)

// declare upserts a copy of the fixture model name with the declared aliases and returns it.
func declare(t *testing.T, c *Catalog, name string, aliases ...string) *model.Model {
	t.Helper()
	found := c.Current().ModelsByName(name)
	if len(found) != 1 {
		t.Fatalf("fixture model %s: got %d, want 1", name, len(found))
	}
	m := *found[0]
	m.Spec.Aliases = aliases
	if err := c.ApplyModelUpsert(&m); err != nil {
		t.Fatalf("upsert %s with aliases %v: %v", name, aliases, err)
	}
	return &m
}

// resolved is what a ref resolves to in the alias index; the zero value is a miss.
type resolved struct {
	Model, Snapshot, Name, Host string
	Pattern                     bool
}

func resolveAlias(s *Snapshot, ref string, patterns bool) resolved {
	r, ok := s.ResolveAlias(slug.From(ref), patterns)
	if !ok {
		return resolved{}
	}
	host, _ := s.HostSlug(r.HostID)
	return resolved{r.Model.Meta.Name, r.Snapshot.Name, r.Name, host, r.Pattern}
}

func TestResolveAlias(t *testing.T) {
	c := catalogFromFixture(t)
	declare(t, c, "gpt-4o", "gpt-4o[1m]", "gpt-4o-mega[*]", "nick*")
	declare(t, c, "gpt-4o-mini", "gpt-4o-mega[exact]", "nick-special*")
	s := c.Current()

	exact := resolved{Model: "gpt-4o", Snapshot: "gpt-4o-2025-01-01", Name: "gpt-4o[1m]"}
	pinned := exact
	pinned.Host = "openai-direct"
	mega := resolved{Model: "gpt-4o", Snapshot: "gpt-4o-2025-01-01", Name: "gpt-4o-mega[*]", Pattern: true}
	for _, tc := range []struct {
		name     string
		ref      string
		patterns bool
		want     resolved
	}{
		{"bare", "gpt-4o[1m]", true, exact},
		{"provider-qualified", "openai/gpt-4o[1m]", true, exact},
		{"host-pinned", "gpt-4o[1m]@openai-direct", false, pinned},
		{"qualified and pinned", "openai/gpt-4o[1m]@openai-direct", false, pinned},
		{"another spelling of the same slug", "GPT-4O.1M", false, exact},
		{"pinned to a host that does not serve it", "gpt-4o[1m]@elsewhere", true, resolved{}},
		{"wildcard", "gpt-4o-mega[ultra-2026]", true, mega},
		{"wildcard needs something after its prefix's dash", "gpt-4o-megax", true, resolved{}},
		{"wildcards off for a pinned ref", "gpt-4o-mega[x]", false, resolved{}},
		{"wildcard never matches a qualified ref", "openai/gpt-4o-mega[x]", true, resolved{}},
		{"exact alias beats another model's wildcard", "gpt-4o-mega[exact]", true,
			resolved{Model: "gpt-4o-mini", Snapshot: "gpt-4o-mini-2025-01-01", Name: "gpt-4o-mega[exact]"}},
		{"longest wildcard prefix wins", "nick-special-thing", true,
			resolved{Model: "gpt-4o-mini", Snapshot: "gpt-4o-mini-2025-01-01", Name: "nick-special*", Pattern: true}},
		{"shorter wildcard takes the rest", "nick-other", true,
			resolved{Model: "gpt-4o", Snapshot: "gpt-4o-2025-01-01", Name: "nick*", Pattern: true}},
		{"a model's own name is not an alias", "gpt-4o", true, resolved{}},
		{"empty ref", "", true, resolved{}},
	} {
		if got := resolveAlias(s, tc.ref, tc.patterns); got != tc.want {
			t.Errorf("%s: ResolveAlias(%q, %v) = %+v, want %+v", tc.name, tc.ref, tc.patterns, got, tc.want)
		}
	}
}

// An alias that normalizes to another model's snapshot name sits in the alias index, but the name still resolves to its own model first: routing asks the alias index only on a miss.
func TestAlias_RealNamesShadowAliases(t *testing.T) {
	c := catalogFromFixture(t)
	declare(t, c, "gpt-4o", "gpt-4o-mini.2025.01.01")
	s := c.Current()
	key := slug.From("gpt-4o-mini-2025-01-01")
	if mod, snap, _, ok := s.ResolveSnapshot(key); !ok || mod.Meta.Name != "gpt-4o-mini" || snap.Name != "gpt-4o-mini-2025-01-01" {
		t.Fatalf("snapshot name resolves to %v (ok %v), want its own model gpt-4o-mini", mod, ok)
	}
	if got := resolveAlias(s, key, true); got.Model != "gpt-4o" {
		t.Fatalf("alias index = %+v, want gpt-4o's entry behind the real name", got)
	}
}

// Two models declaring one alias: the lexicographically smaller model slug holds it, whichever was applied first.
func TestAlias_CrossModelCollisionIsDeterministic(t *testing.T) {
	for _, order := range [][]string{{"gpt-4o", "gpt-4o-mini"}, {"gpt-4o-mini", "gpt-4o"}} {
		c := catalogFromFixture(t)
		for _, name := range order {
			declare(t, c, name, "shared-nick")
		}
		if got := resolveAlias(c.Current(), "shared-nick", true); got.Model != "gpt-4o" {
			t.Errorf("applied in order %v: shared-nick = %+v, want gpt-4o", order, got)
		}
	}
}

func TestAlias_CollisionLoserTakesOverWhenTheWinnerDropsIt(t *testing.T) {
	c := catalogFromFixture(t)
	declare(t, c, "gpt-4o", "shared-nick")
	declare(t, c, "gpt-4o-mini", "shared-nick")
	declare(t, c, "gpt-4o")
	if got := resolveAlias(c.Current(), "shared-nick", true); got.Model != "gpt-4o-mini" {
		t.Errorf("shared-nick = %+v after gpt-4o dropped it, want gpt-4o-mini, which still declares it", got)
	}
}

func TestAlias_CollisionLoserTakesOverWhenTheWinnerIsDeleted(t *testing.T) {
	c := catalogFromFixture(t)
	winner := declare(t, c, "gpt-4o", "shared-nick")
	declare(t, c, "gpt-4o-mini", "shared-nick")
	if err := c.ApplyModelDelete(winner.Meta.ID); err != nil {
		t.Fatal(err)
	}
	if got := resolveAlias(c.Current(), "shared-nick", true); got.Model != "gpt-4o-mini" {
		t.Errorf("shared-nick = %+v after gpt-4o was deleted, want gpt-4o-mini, which still declares it", got)
	}
}

// Re-upserting a model replaces its aliases and deleting it removes them, in the new snapshot only: one taken before keeps resolving what it held.
func TestAlias_ReconcileReplacesAndSweepsWithoutTouchingOlderSnapshots(t *testing.T) {
	c := catalogFromFixture(t)
	declare(t, c, "gpt-4o-mini", "mini[*]")
	m := declare(t, c, "gpt-4o", "gpt-4o[1m]", "gpt-4o-mega[*]")
	first := c.Current()
	declare(t, c, "gpt-4o", "gpt-4o[2m]")
	second := c.Current()
	if err := c.ApplyModelDelete(m.Meta.ID); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		s    *Snapshot
		want map[string]string // ref → model it resolves to, "" for a miss
	}{
		{"before the re-upsert", first, map[string]string{"gpt-4o[1m]": "gpt-4o", "gpt-4o-mega[x]": "gpt-4o", "gpt-4o[2m]": "", "mini[x]": "gpt-4o-mini"}},
		{"after the re-upsert", second, map[string]string{"gpt-4o[1m]": "", "gpt-4o-mega[x]": "", "gpt-4o[2m]": "gpt-4o", "mini[x]": "gpt-4o-mini"}},
		{"after the delete", c.Current(), map[string]string{"gpt-4o[1m]": "", "gpt-4o-mega[x]": "", "gpt-4o[2m]": "", "mini[x]": "gpt-4o-mini"}},
	} {
		for ref, want := range tc.want {
			if got := resolveAlias(tc.s, ref, true); got.Model != want {
				t.Errorf("%s: %s resolves to %q, want %q", tc.name, ref, got.Model, want)
			}
		}
	}
}

// Host-pinned keys are built from the snapshot or alias name and the host name, so a host whose bindings send upstream ids slug.From would mangle still gets them.
func TestResolveModelRef_PinsHostsWhoseBindingsNameTheUpstream(t *testing.T) {
	system := meta.Owner{Kind: meta.OwnerSystem}
	prov := &provider.Provider{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme", Owner: system}}
	m := &model.Model{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "m1", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: prov.Meta.ID}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "m1"}}, Pointer: "m1", Aliases: []string{"m1-latest"}},
	}
	var hosts []*host.Host
	var bindings []*binding.Binding
	for name, upstream := range map[string]string{"amazon-bedrock": "global.acme.m1-v1:0", "google-vertex": "acme/m1@001"} {
		h := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: system}, Spec: host.Spec{BaseURL: "https://" + name + ".example"}}
		hosts = append(hosts, h)
		bindings = append(bindings, &binding.Binding{
			Meta: meta.Metadata{ID: meta.NewID(), Name: "m1-on-" + name, Owner: system},
			Spec: binding.Spec{ModelID: m.Meta.ID, HostID: h.Meta.ID, Adapter: adapters.Anthropic, UpstreamName: upstream},
		})
	}
	s := BuildCatalog([]*provider.Provider{prov}, hosts, []*model.Model{m}, nil, bindings, nil)
	for _, h := range hosts {
		for _, ref := range []string{"m1@" + h.Meta.Name, "acme/m1@" + h.Meta.Name, "m1-latest@" + h.Meta.Name, "acme/m1-latest@" + h.Meta.Name} {
			if res := s.ResolveModelRef(ref); len(res.Models) != 1 || res.Models[0].Meta.ID != m.Meta.ID || res.PinHostID != h.Meta.ID {
				t.Errorf("%s resolves to %+v, want m1 pinned to %s", ref, res, h.Meta.Name)
			}
		}
	}
}

// Validate rejects a pointer naming no snapshot; indexing such a model anyway adds nothing.
func TestAlias_ModelWithoutAPointerSnapshotIndexesNothing(t *testing.T) {
	s := emptySnap().clone()
	m := *catalogFromFixture(t).Current().ModelsByName("gpt-4o")[0]
	m.Spec.Aliases, m.Spec.Pointer = []string{"gpt-4o[1m]", "gpt-4o-mega[*]"}, "nope"
	s.indexModelAliases(&m, "openai")
	if len(s.aliasExact) != 0 || len(s.aliasPatterns) != 0 {
		t.Errorf("indexed %v and %v for a model without a pointer snapshot", s.aliasExact, s.aliasPatterns)
	}
}
