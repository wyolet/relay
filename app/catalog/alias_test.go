package catalog

import (
	"testing"

	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/pkg/slug"
)

// aliasedFixtureModel returns the fixture's gpt-4o model with declared
// aliases added: one exact bracket variant and one wildcard pattern.
func aliasedFixtureModel(t *testing.T, c *Catalog) *model.Model {
	t.Helper()
	s := c.Current()
	orig := s.ModelsByName("gpt-4o")
	if len(orig) != 1 {
		t.Fatalf("fixture model: got %d, want 1", len(orig))
	}
	m := *orig[0]
	m.Spec.Aliases = []string{"gpt-4o[1m]", "gpt-4o-mega[*]"}
	if err := m.Validate(); err != nil {
		t.Fatalf("fixture alias model invalid: %v", err)
	}
	return &m
}

func TestAlias_ExactFormsSynthesized(t *testing.T) {
	c := catalogFromFixture(t)
	m := aliasedFixtureModel(t, c)
	if err := c.ApplyModelUpsert(m); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	s := c.Current()

	for _, ref := range []string{
		"gpt-4o[1m]",                      // bare
		"openai/gpt-4o[1m]",               // provider-qualified
		"gpt-4o[1m]@openai-direct",        // host-pinned
		"openai/gpt-4o[1m]@openai-direct", // both
		"GPT-4O.1M",                       // normalization collapses with bare
	} {
		got, ok := s.ResolveAlias(slug.From(ref), true)
		if !ok {
			t.Fatalf("ResolveAlias(%q): no match", ref)
		}
		if got.Model.Meta.ID != m.Meta.ID {
			t.Errorf("ResolveAlias(%q): wrong model %q", ref, got.Model.Meta.Name)
		}
		if got.Name != "gpt-4o[1m]" {
			t.Errorf("ResolveAlias(%q): Name = %q, want declared alias", ref, got.Name)
		}
		if got.Pattern {
			t.Errorf("ResolveAlias(%q): unexpectedly a pattern match", ref)
		}
		if got.Snapshot == nil || got.Snapshot.Name != "gpt-4o-2025-01-01" {
			t.Errorf("ResolveAlias(%q): target snapshot != pointer", ref)
		}
	}

	// Pinned form carries the host id.
	pinned, ok := s.ResolveAlias(slug.From("gpt-4o[1m]@openai-direct"), true)
	if !ok || pinned.HostID == "" {
		t.Fatalf("pinned form: ok=%v hostID=%q, want pin", ok, pinned.HostID)
	}
	bare, _ := s.ResolveAlias(slug.From("gpt-4o[1m]"), true)
	if bare.HostID != "" {
		t.Errorf("bare form: hostID = %q, want empty", bare.HostID)
	}
}

func TestAlias_PatternMatchAndPrecedence(t *testing.T) {
	c := catalogFromFixture(t)
	m := aliasedFixtureModel(t, c)
	if err := c.ApplyModelUpsert(m); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	s := c.Current()

	// Pattern matches an arbitrary suffix form.
	got, ok := s.ResolveAlias(slug.From("gpt-4o-mega[ultra-2026]"), true)
	if !ok || !got.Pattern || got.Name != "gpt-4o-mega[*]" {
		t.Fatalf("pattern match: ok=%v pattern=%v name=%q", ok, got.Pattern, got.Name)
	}
	// Boundary: the pattern prefix keeps its dash, so a glued continuation
	// of the literal must NOT match ("gpt-4o-megaX...").
	if _, ok := s.ResolveAlias(slug.From("gpt-4o-megax"), true); ok {
		t.Error("boundary: gpt-4o-megax matched gpt-4o-mega[*]")
	}
	// patterns=false (pinned refs) skips the wildcard list.
	if _, ok := s.ResolveAlias(slug.From("gpt-4o-mega[x]"), false); ok {
		t.Error("patterns=false still matched a wildcard")
	}
	// Exact alias beats pattern: declare an exact alias inside the
	// pattern's range on the OTHER model.
	s2 := c.Current()
	other := s2.ModelsByName("gpt-4o-mini")
	if len(other) != 1 {
		t.Fatalf("fixture mini model missing")
	}
	mm := *other[0]
	mm.Spec.Aliases = []string{"gpt-4o-mega[exact]"}
	if err := c.ApplyModelUpsert(&mm); err != nil {
		t.Fatalf("upsert mini: %v", err)
	}
	got, ok = c.Current().ResolveAlias(slug.From("gpt-4o-mega[exact]"), true)
	if !ok || got.Pattern || got.Model.Meta.ID != mm.Meta.ID {
		t.Fatalf("exact-beats-pattern: ok=%v pattern=%v model=%q", ok, got.Pattern, got.Model.Meta.Name)
	}
}

func TestAlias_RealNamesShadowAliases(t *testing.T) {
	// An alias on model A normalizing to a snapshot name of model B must
	// lose: routing probes ResolveSnapshot first. Snapshot-level guarantee:
	// the snapshot name is still resolvable and the alias entry coexists.
	c := catalogFromFixture(t)
	m := aliasedFixtureModel(t, c)
	m.Spec.Aliases = []string{"gpt-4o-mini.2025.01.01"} // normalizes to mini's snapshot name
	if err := c.ApplyModelUpsert(m); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	s := c.Current()
	key := slug.From("gpt-4o-mini-2025-01-01")
	if mod, _, _, ok := s.ResolveSnapshot(key); !ok || mod.Meta.Name != "gpt-4o-mini" {
		t.Fatalf("real snapshot name no longer resolves to its model")
	}
	// The alias entry exists but routing never reaches it for this key.
	if ref, ok := s.ResolveAlias(key, true); !ok || ref.Model.Meta.Name != "gpt-4o" {
		t.Fatalf("alias entry missing from last-priority index")
	}
}

func TestAlias_CrossModelCollisionDeterministic(t *testing.T) {
	// Apply in both orders; the winner must be identical (lexicographic
	// smaller model slug: "gpt-4o" < "gpt-4o-mini").
	run := func(order ...string) string {
		c := catalogFromFixture(t)
		for _, name := range order {
			m := *c.Current().ModelsByName(name)[0]
			m.Spec.Aliases = []string{"shared-nick"}
			if err := c.ApplyModelUpsert(&m); err != nil {
				t.Fatal(err)
			}
		}
		got, ok := c.Current().ResolveAlias(slug.From("shared-nick"), true)
		if !ok {
			t.Fatal("collision: no match")
		}
		return got.Model.Meta.Name
	}
	if w := run("gpt-4o", "gpt-4o-mini"); w != "gpt-4o" {
		t.Fatalf("order a,b: winner %q, want gpt-4o", w)
	}
	if w := run("gpt-4o-mini", "gpt-4o"); w != "gpt-4o" {
		t.Fatalf("order b,a: winner %q, want gpt-4o", w)
	}
}

func TestAlias_OverlappingPatternsLongestPrefixWins(t *testing.T) {
	c := catalogFromFixture(t)
	s0 := c.Current()
	a := *s0.ModelsByName("gpt-4o")[0]
	b := *s0.ModelsByName("gpt-4o-mini")[0]
	a.Spec.Aliases = []string{"nick*"}         // prefix "nick"
	b.Spec.Aliases = []string{"nick-special*"} // longer prefix
	if err := c.ApplyModelUpsert(&a); err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyModelUpsert(&b); err != nil {
		t.Fatal(err)
	}
	s := c.Current()
	got, ok := s.ResolveAlias(slug.From("nick-special-thing"), true)
	if !ok || got.Model.Meta.ID != b.Meta.ID {
		t.Fatalf("longest prefix: got %q, want gpt-4o-mini", got.Model.Meta.Name)
	}
	got, ok = s.ResolveAlias(slug.From("nick-other"), true)
	if !ok || got.Model.Meta.ID != a.Meta.ID {
		t.Fatalf("short prefix: got %q, want gpt-4o", got.Model.Meta.Name)
	}
}

func TestAlias_ReconcileSweepsOnDelete(t *testing.T) {
	c := catalogFromFixture(t)
	m := aliasedFixtureModel(t, c)
	if err := c.ApplyModelUpsert(m); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Current().ResolveAlias(slug.From("gpt-4o[1m]"), true); !ok {
		t.Fatal("precondition: alias resolvable")
	}
	if err := c.ApplyModelDelete(m.Meta.ID); err != nil {
		t.Fatal(err)
	}
	s := c.Current()
	if _, ok := s.ResolveAlias(slug.From("gpt-4o[1m]"), true); ok {
		t.Error("exact alias survived model delete")
	}
	if _, ok := s.ResolveAlias(slug.From("gpt-4o-mega[x]"), true); ok {
		t.Error("pattern alias survived model delete")
	}
}

func TestAlias_ReconcileReindexesOnUpsert(t *testing.T) {
	c := catalogFromFixture(t)
	m := aliasedFixtureModel(t, c)
	if err := c.ApplyModelUpsert(m); err != nil {
		t.Fatal(err)
	}
	// Replace the alias set; the old entries must vanish.
	m2 := *m
	m2.Spec.Aliases = []string{"gpt-4o[2m]"}
	if err := c.ApplyModelUpsert(&m2); err != nil {
		t.Fatal(err)
	}
	s := c.Current()
	if _, ok := s.ResolveAlias(slug.From("gpt-4o[1m]"), true); ok {
		t.Error("stale exact alias survived re-upsert")
	}
	if _, ok := s.ResolveAlias(slug.From("gpt-4o-mega[x]"), true); ok {
		t.Error("stale pattern alias survived re-upsert")
	}
	if _, ok := s.ResolveAlias(slug.From("gpt-4o[2m]"), true); !ok {
		t.Error("new alias not indexed")
	}
}

func TestAlias_CloneIsolation(t *testing.T) {
	c := catalogFromFixture(t)
	m := aliasedFixtureModel(t, c)
	if err := c.ApplyModelUpsert(m); err != nil {
		t.Fatal(err)
	}
	before := c.Current()
	// Mutate via reconcile (clones internally); the captured snapshot
	// must keep resolving both alias kinds.
	if err := c.ApplyModelDelete(m.Meta.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := before.ResolveAlias(slug.From("gpt-4o[1m]"), true); !ok {
		t.Error("old snapshot lost exact alias after reconcile on the catalog")
	}
	if _, ok := before.ResolveAlias(slug.From("gpt-4o-mega[x]"), true); !ok {
		t.Error("old snapshot lost pattern alias after reconcile on the catalog")
	}
}

func TestAlias_PointerTargetMissingIsSkipped(t *testing.T) {
	// Defensive path: a model whose Pointer matches no snapshot (Validate
	// rejects this, and reconcile validates — but indexing stays
	// defensive) must not index aliases or panic.
	s := emptySnap().clone()
	m := aliasedFixtureModel(t, catalogFromFixture(t))
	m.Spec.Pointer = "nope"
	s.indexModelAliases(m, "")
	if len(s.aliasExact) != 0 || len(s.aliasPatterns) != 0 {
		t.Error("alias indexed despite missing pointer snapshot")
	}
}
