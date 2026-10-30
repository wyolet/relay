package engine_test

import (
	"slices"
	"testing"

	"github.com/wyolet/relay/pkg/engine"
)

// binding names a team by slug, like a policy binding naming a group.
type binding struct{ ID, TeamName string }

type bindingPlan struct{ Team *team }

// index is a singleton derived object over every team, like a catalog index.
type index struct{ Names []string }

func withBindings(f *fixture, rows ...*binding) *src[binding] {
	s := newSrc(func(b *binding) string { return b.ID }, rows...)
	engine.RegisterKind(f.e, engine.KindSpec[binding]{Kind: "binding", Source: s, Meta: func(b *binding) engine.Meta {
		return engine.Meta{ID: b.ID}
	}})
	engine.RegisterKind(f.e, engine.KindSpec[bindingPlan]{Kind: "bindingplan"})
	c := engine.NewController("binding-plan", "bindingplan", func(tx *engine.Txn, id string) error {
		f.count("binding-plan/" + id)
		b := engine.Get[binding](tx, id)
		if b == nil {
			engine.Drop(tx)
			return nil
		}
		engine.Put(tx, &bindingPlan{Team: engine.ByNameOf[team](tx, b.TeamName)})
		return nil
	})
	engine.Watch(c, func(b *binding) []string { return []string{b.ID} })
	f.e.Register(c)
	return s
}

func TestByNameEdgesFollowRenames(t *testing.T) {
	f := newFixture(t, engine.Options{}, func(f *fixture) {
		withBindings(f, &binding{ID: "b1", TeamName: "delta"}, &binding{ID: "b2", TeamName: "alpha"})
	})
	v := f.view()
	if engine.Get[bindingPlan](v, "b1").Team != nil || engine.Get[bindingPlan](v, "b2").Team.ID != "t1" {
		t.Fatal("initial name resolution wrong")
	}
	if refs := v.Referrers(engine.ByName("team", "delta")); !slices.Equal(refs, []engine.Ref{{Kind: "bindingplan", ID: "b1"}}) {
		t.Fatalf("referrers of an unused name = %v", refs)
	}

	// Renaming alpha → delta dirties both names' readers.
	mustApply(t, f.e, &team{ID: "t1", Name: "delta", Limit: 10})
	v = f.view()
	if engine.Get[bindingPlan](v, "b1").Team.ID != "t1" || engine.Get[bindingPlan](v, "b2").Team != nil {
		t.Fatal("rename not followed by name readers")
	}

	// A value change without a rename still reaches name readers.
	mustApply(t, f.e, &team{ID: "t1", Name: "delta", Limit: 99})
	if engine.Get[bindingPlan](f.view(), "b1").Team.Limit != 99 {
		t.Fatal("name reader missed a value change")
	}
}

func TestDuplicateNamesResolveToNeither(t *testing.T) {
	f := newFixture(t, engine.Options{}, func(f *fixture) {
		withBindings(f, &binding{ID: "b1", TeamName: "alpha"})
	})
	mustApply(t, f.e, &team{ID: "t3", Name: "alpha"})
	v := f.view()
	if engine.ByNameOf[team](v, "alpha") != nil || engine.Get[bindingPlan](v, "b1").Team != nil {
		t.Fatal("a duplicate name resolved")
	}
	for _, id := range []string{"t1", "t3"} {
		is := v.Issues(engine.Ref{Kind: "team", ID: id})
		if len(is) != 1 || is[0].Code != "duplicate_name" || is[0].Severity != engine.SeverityError {
			t.Fatalf("%s issues = %+v", id, is)
		}
	}

	mustApply(t, f.e, &team{ID: "t3", Name: "omega"})
	v = f.view()
	if len(v.AllIssues()) != 0 {
		t.Fatalf("issues after the rename = %+v", v.AllIssues())
	}
	if engine.Get[bindingPlan](v, "b1").Team.ID != "t1" {
		t.Fatal("name did not resolve again after the duplicate left")
	}
}

func TestAnyEdgeDirtiesSingleton(t *testing.T) {
	f := newFixture(t, engine.Options{}, func(f *fixture) {
		engine.RegisterKind(f.e, engine.KindSpec[index]{Kind: "index"})
		c := engine.NewController("index", "index", func(tx *engine.Txn, _ string) error {
			f.count("index")
			var ix index
			for tm := range engine.All[team](tx) {
				ix.Names = append(ix.Names, tm.Name)
			}
			engine.Put(tx, &ix)
			return nil
		})
		engine.Watch(c, func(*team) []string { return []string{""} })
		f.e.Register(c)

		engine.RegisterKind(f.e, engine.KindSpec[int]{Kind: "indexsize"})
		s := engine.NewController("index-size", "indexsize", func(tx *engine.Txn, id string) error {
			n := len(engine.Get[index](tx, "").Names)
			engine.Put(tx, &n)
			return nil
		})
		engine.Watch(s, func(u *user) []string { return []string{u.ID} })
		f.e.Register(s)
	})
	v := f.view()
	if got := engine.Get[index](v, "").Names; !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Fatalf("index = %v", got)
	}
	if got := v.Referrers(engine.ByID("index", "")); len(got) != 3 {
		t.Fatalf("referrers of the singleton = %v", got)
	}
	if got := v.Referrers(engine.Any("team")); got != nil {
		t.Fatalf("Any referrers = %v, want nil", got)
	}

	mustApply(t, f.e, &team{ID: "t5", Name: "epsilon"})
	v = f.view()
	if got := *engine.Get[int](v, "u1"); got != 3 {
		t.Fatalf("index size seen by u1 = %d", got)
	}
}

func TestReferrersAfterEdgeChange(t *testing.T) {
	f := newFixture(t, engine.Options{}, nil)
	mustApply(t, f.e, &user{ID: "u1", Name: "ann", TeamID: "t2"})
	v := f.view()
	if refs := v.Referrers(engine.ByID("team", "t1")); slices.Contains(refs, engine.Ref{Kind: "userplan", ID: "u1"}) {
		t.Fatalf("old edge kept: %v", refs)
	}
	if refs := v.Referrers(engine.ByID("team", "t2")); !slices.Contains(refs, engine.Ref{Kind: "userplan", ID: "u1"}) {
		t.Fatalf("new edge missing: %v", refs)
	}
}
