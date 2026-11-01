package engine_test

import (
	"slices"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/engine"
)

func TestLoadPublishesGenerationOne(t *testing.T) {
	f := newFixture(t, engine.Options{}, nil)
	v := f.view()
	if v.Gen() != 1 {
		t.Fatalf("gen = %d, want 1", v.Gen())
	}
	up := engine.Get[userPlan](v, "u1")
	if up == nil || up.Team.ID != "t1" {
		t.Fatalf("userplan u1 = %+v", up)
	}
	tp := engine.Get[teamPlan](v, "t1")
	if tp == nil || len(tp.Users) != 2 || tp.Users[0].ID != "u1" || tp.Users[1].ID != "u2" {
		t.Fatalf("teamplan t1 = %+v", tp)
	}
	if got := engine.ByNameOf[team](v, "beta"); got == nil || got.ID != "t2" {
		t.Fatalf("ByNameOf beta = %+v", got)
	}
	var ids []string
	for u := range engine.All[user](v) {
		ids = append(ids, u.ID)
	}
	if !slices.Equal(ids, []string{"u1", "u2", "u3"}) {
		t.Fatalf("All users = %v", ids)
	}
	if s := f.lastStats(); s.Gen != 1 || s.Inputs != 5 || s.Reconciled != 5 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestApplyRebuildsOnlyDependents(t *testing.T) {
	f := newFixture(t, engine.Options{}, nil)
	before := f.view()
	u3 := engine.Get[userPlan](before, "u3")
	f.resetCalls()

	mustApply(t, f.e, &team{ID: "t1", Name: "alpha", Limit: 11})
	after := f.view()
	if after.Gen() != 2 {
		t.Fatalf("gen = %d, want 2", after.Gen())
	}
	if got := engine.Get[userPlan](after, "u1").Team.Limit; got != 11 {
		t.Fatalf("u1 sees limit %d", got)
	}
	if engine.Get[userPlan](after, "u3") != u3 {
		t.Fatal("u3 plan replaced by an unrelated team write")
	}
	if f.callsOf("user-plan/u3") != 0 || f.callsOf("user-plan/u1") != 1 || f.callsOf("team-plan/t1") != 1 {
		t.Fatalf("calls = %v", f.calls)
	}
	// The previous generation is untouched.
	if engine.Get[userPlan](before, "u1").Team.Limit != 10 || before.Gen() != 1 {
		t.Fatal("published view changed after a later write")
	}
}

func TestHookMayApply(t *testing.T) {
	var gens []engine.Generation
	f := newFixture(t, engine.Options{}, func(f *fixture) {
		f.onPublish = func(v *engine.View) {
			gens = append(gens, v.Gen())
			if v.Gen() == 2 {
				if err := engine.Apply(f.e, &team{ID: "t2", Name: "beta", Limit: 21}); err != nil {
					t.Errorf("Apply from the hook: %v", err)
				}
			}
		}
	})
	done := make(chan error, 1)
	go func() { done <- engine.Apply(f.e, &team{ID: "t1", Name: "alpha", Limit: 11}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Apply from a hook did not complete")
	}
	if !slices.Equal(gens, []engine.Generation{1, 2, 3}) {
		t.Fatalf("hook saw generations %v", gens)
	}
	if got := engine.Get[userPlan](f.view(), "u3").Team.Limit; got != 21 {
		t.Fatalf("u3 sees limit %d after the hook's write", got)
	}
}

func TestUnchangedOutputStopsCascade(t *testing.T) {
	f := newFixture(t, engine.Options{}, func(f *fixture) {
		engine.RegisterKind(f.e, engine.KindSpec[string]{Kind: "label"})
		c := engine.NewController("label", "label", func(tx *engine.Txn, id string) error {
			f.count("label/" + id)
			up := engine.Get[userPlan](tx, id)
			if up == nil {
				engine.Drop(tx)
				return nil
			}
			s := up.User.Name + "@" + up.Team.Name
			engine.Put(tx, &s)
			return nil
		})
		engine.Watch(c, func(u *user) []string { return []string{u.ID} })
		f.e.Register(c)
	})
	f.resetCalls()
	// teamplan t2 changes (Members re-read), userplan u3 returns its previous
	// pointer, so the label reading userplan u3 is not reconciled.
	mustApply(t, f.e, &team{ID: "t2", Name: "beta", Limit: 20})
	if f.callsOf("user-plan/u3") != 1 || f.callsOf("label/u3") != 0 {
		t.Fatalf("calls = %v", f.calls)
	}
	mustApply(t, f.e, &team{ID: "t2", Name: "gamma", Limit: 20})
	if got := *engine.Get[string](f.view(), "u3"); got != "cy@gamma" || f.callsOf("label/u3") != 1 {
		t.Fatalf("label = %q, calls = %v", got, f.calls)
	}
}

func TestParkedRowBuildsWhenParentArrives(t *testing.T) {
	f := newFixture(t, engine.Options{}, nil)
	mustApply(t, f.e, &user{ID: "u4", Name: "dee", TeamID: "t9"})
	v := f.view()
	if engine.Get[userPlan](v, "u4") != nil {
		t.Fatal("plan built without its team")
	}
	if engine.Get[user](v, "u4") == nil {
		t.Fatal("parked input row left the view")
	}
	is := v.Issues(engine.Ref{Kind: "userplan", ID: "u4"})
	if len(is) != 1 || is[0].Code != "ref_missing" || is[0].Target.ID != "t9" {
		t.Fatalf("issues = %+v", is)
	}
	if refs := v.Referrers(engine.ByID("team", "t9")); !slices.Equal(refs, []engine.Ref{{Kind: "teamplan", ID: "t9"}, {Kind: "userplan", ID: "u4"}}) {
		t.Fatalf("referrers of missing team = %v", refs)
	}

	mustApply(t, f.e, &team{ID: "t9", Name: "nine"})
	v = f.view()
	if up := engine.Get[userPlan](v, "u4"); up == nil || up.Team.ID != "t9" {
		t.Fatalf("plan after parent arrived = %+v", up)
	}
	if is := v.Issues(engine.Ref{Kind: "userplan", ID: "u4"}); len(is) != 0 {
		t.Fatalf("issues not cleared: %+v", is)
	}
}

func TestDeleteCascadesAndForgets(t *testing.T) {
	f := newFixture(t, engine.Options{}, nil)
	if err := engine.Delete(f.e, engine.Ref{Kind: "team", ID: "t1"}); err != nil {
		t.Fatal(err)
	}
	v := f.view()
	if engine.Get[userPlan](v, "u1") != nil || engine.Get[teamPlan](v, "t1") != nil {
		t.Fatal("dependents of a deleted team still published")
	}
	if got := codes(v.Issues(engine.Ref{Kind: "userplan", ID: "u2"})); !slices.Equal(got, []string{"ref_missing"}) {
		t.Fatalf("u2 issues = %v", got)
	}

	if err := engine.Delete(f.e, engine.Ref{Kind: "user", ID: "u2"}); err != nil {
		t.Fatal(err)
	}
	v = f.view()
	if is := v.Issues(engine.Ref{Kind: "userplan", ID: "u2"}); len(is) != 0 {
		t.Fatalf("a deleted row's plan kept issues: %+v", is)
	}
	if refs := v.Referrers(engine.ByID("user", "u2")); refs != nil {
		t.Fatalf("a forgotten output kept edges: %v", refs)
	}
}

func TestMembershipMovesBetweenGroups(t *testing.T) {
	f := newFixture(t, engine.Options{}, nil)
	f.resetCalls()
	mustApply(t, f.e, &user{ID: "u2", Name: "bob", TeamID: "t2"})
	v := f.view()
	if got := len(engine.Get[teamPlan](v, "t1").Users); got != 1 {
		t.Fatalf("t1 has %d users", got)
	}
	if got := engine.Get[teamPlan](v, "t2").Users; len(got) != 2 || got[0].ID != "u2" || got[1].ID != "u3" {
		t.Fatalf("t2 users = %+v", got)
	}
	if f.callsOf("team-plan/t1") != 1 || f.callsOf("team-plan/t2") != 1 {
		t.Fatalf("calls = %v", f.calls)
	}
}

func TestViewsAreSafeToReadDuringWrites(t *testing.T) {
	f := newFixture(t, engine.Options{}, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 200 {
			if err := engine.Apply(f.e, &team{ID: "t1", Name: "alpha", Limit: i}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for {
		select {
		case <-done:
			if got := engine.Get[userPlan](f.view(), "u1").Team.Limit; got != 199 {
				t.Fatalf("final limit = %d", got)
			}
			return
		default:
		}
		v := f.view()
		up := engine.Get[userPlan](v, "u1")
		if up == nil || up.Team != engine.Get[team](v, "t1") {
			t.Fatal("a view mixed two generations")
		}
		_ = v.Referrers(engine.ByID("team", "t1"))
	}
}
