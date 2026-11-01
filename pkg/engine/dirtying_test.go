package engine_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/wyolet/relay/pkg/engine"
)

func TestByIDReaderRebuildsOnlyForItsRow(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.parents.set(&parent{ID: "p1", Ver: 1, Val: 1}, &parent{ID: "p2", Ver: 1, Val: 1})
	r.items.set(&item{ID: "i1", Ver: 1, Parent: "p1"})
	r.e.Register(ctlByID(r))
	r.load()

	r.putParent(&parent{ID: "p2", Ver: 2, Val: 9})
	if n := r.calls.count("by-id", "i1"); n != 0 {
		t.Fatalf("unrelated write reconciled i1 %d times", n)
	}
	r.putParent(&parent{ID: "p1", Ver: 2, Val: 5})
	if n := r.calls.count("by-id", "i1"); n != 1 {
		t.Fatalf("parent upsert reconciled i1 %d times, want 1", n)
	}
	if got := viewA(r.view(), "i1"); got == nil || got.V != 5 {
		t.Fatalf("i1 output = %+v, want V=5", got)
	}
}

func TestByIDReaderRebuildsOnDelete(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.parents.set(&parent{ID: "p1", Ver: 1, Val: 1})
	r.items.set(&item{ID: "i1", Ver: 1, Parent: "p1"})
	r.e.Register(ctlByID(r))
	r.load()

	r.delParent("p1")
	if n := r.calls.count("by-id", "i1"); n != 1 {
		t.Fatalf("parent delete reconciled i1 %d times, want 1", n)
	}
	if viewA(r.view(), "i1") != nil {
		t.Fatal("output survived the delete of its parent")
	}
}

func TestReadOfMissingRowRecordsAnEdge(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.items.set(&item{ID: "i1", Ver: 1, Parent: "p1"})
	r.e.Register(ctlByID(r))
	r.load()
	if viewA(r.view(), "i1") != nil {
		t.Fatal("output built without its parent")
	}
	r.putParent(&parent{ID: "p1", Ver: 1, Val: 3})
	if got := viewA(r.view(), "i1"); got == nil || got.V != 3 {
		t.Fatalf("output after parent arrived = %+v, want V=3", got)
	}
}

func TestRowWithMissingParentStaysVisibleWithIssue(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.items.set(&item{ID: "i1", Ver: 1, Parent: "p1"})
	r.e.Register(ctlByID(r))
	r.load()

	v := r.view()
	if engine.Get[item](v, "i1") == nil {
		t.Fatal("input row with a missing parent is not in the view")
	}
	is := v.Issues(refOf(kItem, "i1"))
	if len(is) != 1 || is[0].Code != "ref_missing" || is[0].Severity != engine.SeverityError || is[0].Target != refOf(kParent, "p1") {
		t.Fatalf("issues on parked row = %+v, want one ref_missing error targeting p1", is)
	}

	r.putParent(&parent{ID: "p1", Ver: 1, Val: 1})
	if got := r.view().Issues(refOf(kItem, "i1")); len(got) != 0 {
		t.Fatalf("issues after parent arrived = %v, want none", issueCodes(got))
	}
}

func TestByNameReaderRebuildsOnValueChange(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.parents.set(&parent{ID: "p1", Name: "alpha", Ver: 1, Val: 1})
	r.items.set(&item{ID: "i1", Ver: 1, ParentName: "alpha"})
	r.e.Register(ctlByName(r))
	r.load()

	r.putParent(&parent{ID: "p1", Name: "alpha", Ver: 2, Val: 7})
	if n := r.calls.count("by-name", "i1"); n != 1 {
		t.Fatalf("value change reconciled the name reader %d times, want 1", n)
	}
	if got := viewB(r.view(), "i1"); got == nil || got.V != 7 {
		t.Fatalf("name reader output = %+v, want V=7", got)
	}
}

func TestRenameDirtiesReadersOfBothNames(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.parents.set(&parent{ID: "p1", Name: "alpha", Ver: 1, Val: 1})
	r.items.set(
		&item{ID: "old", Ver: 1, ParentName: "alpha"},
		&item{ID: "new", Ver: 1, ParentName: "beta"},
		&item{ID: "other", Ver: 1, ParentName: "gamma"},
	)
	r.e.Register(ctlByName(r))
	r.load()

	r.putParent(&parent{ID: "p1", Name: "beta", Ver: 2, Val: 1})
	if r.calls.count("by-name", "old") != 1 || r.calls.count("by-name", "new") != 1 {
		t.Fatalf("rename reconciled old-name reader %d, new-name reader %d times, want 1 each", r.calls.count("by-name", "old"), r.calls.count("by-name", "new"))
	}
	if n := r.calls.count("by-name", "other"); n != 0 {
		t.Fatalf("rename reconciled an unrelated name reader %d times", n)
	}
	v := r.view()
	if got := viewB(v, "old"); got == nil || got.V != -1 {
		t.Fatalf("old-name reader = %+v, want unresolved", got)
	}
	if got := viewB(v, "new"); got == nil || got.S != "p1" {
		t.Fatalf("new-name reader = %+v, want resolved to p1", got)
	}
}

func TestNameReaderResolvesRowCreatedLater(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.items.set(&item{ID: "i1", Ver: 1, ParentName: "alpha"})
	r.e.Register(ctlByName(r))
	r.load()

	r.putParent(&parent{ID: "p1", Name: "alpha", Ver: 1, Val: 4})
	if got := viewB(r.view(), "i1"); got == nil || got.S != "p1" || got.V != 4 {
		t.Fatalf("name reader after create = %+v, want p1 with V=4", got)
	}
}

func TestNameReuseByAnotherRowDirtiesReader(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.parents.set(&parent{ID: "p1", Name: "alpha", Ver: 1, Val: 1})
	r.items.set(&item{ID: "i1", Ver: 1, ParentName: "alpha"})
	r.e.Register(ctlByName(r))
	r.load()

	r.delParent("p1")
	if got := viewB(r.view(), "i1"); got == nil || got.V != -1 {
		t.Fatalf("name reader after delete = %+v, want unresolved", got)
	}
	r.putParent(&parent{ID: "p2", Name: "alpha", Ver: 1, Val: 2})
	if got := viewB(r.view(), "i1"); got == nil || got.S != "p2" {
		t.Fatalf("name reader after reuse = %+v, want p2", got)
	}

	// p3 takes the name by rename rather than create.
	r.putParent(&parent{ID: "p2", Name: "other", Ver: 2, Val: 2})
	r.putParent(&parent{ID: "p3", Name: "unrelated", Ver: 1, Val: 3})
	r.calls.reset()
	r.putParent(&parent{ID: "p3", Name: "alpha", Ver: 2, Val: 3})
	if n := r.calls.count("by-name", "i1"); n != 1 {
		t.Fatalf("rename onto a read name reconciled the reader %d times, want 1", n)
	}
	if got := viewB(r.view(), "i1"); got == nil || got.S != "p3" {
		t.Fatalf("name reader after rename onto its name = %+v, want p3", got)
	}
}

func TestDuplicateNameResolvesToNeitherAndFlagsBothRows(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.parents.set(&parent{ID: "p1", Name: "dup", Ver: 1, Val: 1}, &parent{ID: "p2", Name: "dup", Ver: 1, Val: 2})
	r.items.set(&item{ID: "i1", Ver: 1, ParentName: "dup"})
	r.e.Register(ctlByName(r))
	r.load()

	v := r.view()
	if engine.ByNameOf[parent](v, "dup") != nil {
		t.Fatal("duplicate name resolved")
	}
	if got := viewB(v, "i1"); got == nil || got.V != -1 {
		t.Fatalf("reader of a duplicate name = %+v, want unresolved", got)
	}
	for _, id := range []string{"p1", "p2"} {
		if !slices.Contains(issueCodes(v.Issues(refOf(kParent, id))), "duplicate_name") {
			t.Fatalf("issues on %s = %v, want duplicate_name", id, issueCodes(v.Issues(refOf(kParent, id))))
		}
	}

	r.delParent("p1")
	v = r.view()
	if got := viewB(v, "i1"); got == nil || got.S != "p2" {
		t.Fatalf("reader after duplicate removed = %+v, want p2", got)
	}
	if got := v.Issues(refOf(kParent, "p2")); slices.Contains(issueCodes(got), "duplicate_name") {
		t.Fatal("duplicate_name kept after the collision ended")
	}
}

func TestAnyReaderRebuildsOnEveryRowOfTheKind(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.items.set(&item{ID: "i1", Ver: 1})
	c := engine.NewController("count", kA, func(tx *engine.Txn, id string) error {
		r.calls.hit("count", id)
		n := 0
		for range engine.All[parent](tx) {
			n++
		}
		engine.Put(tx, &outA{V: n})
		return nil
	})
	engine.Watch(c, selfKey)
	r.e.Register(c)
	r.load()

	r.putParent(&parent{ID: "p1", Ver: 1})
	r.putParent(&parent{ID: "p2", Ver: 1})
	if n := r.calls.count("count", "i1"); n != 2 {
		t.Fatalf("kind-wide reader reconciled %d times for 2 writes, want 2", n)
	}
	if got := viewA(r.view(), "i1"); got == nil || got.V != 2 {
		t.Fatalf("kind-wide reader = %+v, want V=2", got)
	}
	r.delParent("p1")
	if got := viewA(r.view(), "i1"); got == nil || got.V != 1 {
		t.Fatalf("kind-wide reader after delete = %+v, want V=1", got)
	}
}

// ctlGroups builds one outB per Group value, listing its member ids.
func ctlGroups(r *rig) *engine.Controller {
	c := engine.NewController("groups", kB, func(tx *engine.Txn, id string) error {
		r.calls.hit("groups", id)
		ms := engine.Members[item](tx)
		if len(ms) == 0 {
			engine.Drop(tx)
			return nil
		}
		ids := make([]string, 0, len(ms))
		sum := 0
		for _, m := range ms {
			ids = append(ids, m.ID)
			sum += m.Val
		}
		engine.Put(tx, &outB{V: sum, S: strings.Join(ids, ",")})
		return nil
	})
	engine.Watch(c, func(i *item) []string { return []string{i.Group} })
	return c
}

func TestMembershipMoveDirtiesOldAndNewGroup(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.items.set(
		&item{ID: "i1", Ver: 1, Group: "g1"},
		&item{ID: "i2", Ver: 1, Group: "g1"},
		&item{ID: "i3", Ver: 1, Group: "g3"},
	)
	r.e.Register(ctlGroups(r))
	r.load()

	r.putItem(&item{ID: "i2", Ver: 2, Group: "g2"})
	if r.calls.count("groups", "g1") != 1 || r.calls.count("groups", "g2") != 1 {
		t.Fatalf("move reconciled g1 %d, g2 %d times, want 1 each", r.calls.count("groups", "g1"), r.calls.count("groups", "g2"))
	}
	if n := r.calls.count("groups", "g3"); n != 0 {
		t.Fatalf("move reconciled an unrelated group %d times", n)
	}
	v := r.view()
	if got := viewB(v, "g1"); got == nil || got.S != "i1" {
		t.Fatalf("g1 = %+v, want members i1", got)
	}
	if got := viewB(v, "g2"); got == nil || got.S != "i2" {
		t.Fatalf("g2 = %+v, want members i2", got)
	}
}

func TestMemberValueChangeDirtiesItsGroup(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.items.set(&item{ID: "i1", Ver: 1, Group: "g1", Val: 1}, &item{ID: "i2", Ver: 1, Group: "g2", Val: 1})
	r.e.Register(ctlGroups(r))
	r.load()

	r.putItem(&item{ID: "i1", Ver: 2, Group: "g1", Val: 5})
	if r.calls.count("groups", "g1") != 1 || r.calls.count("groups", "g2") != 0 {
		t.Fatalf("value change reconciled g1 %d, g2 %d times, want 1 and 0", r.calls.count("groups", "g1"), r.calls.count("groups", "g2"))
	}
	if got := viewB(r.view(), "g1"); got == nil || got.V != 5 {
		t.Fatalf("g1 = %+v, want V=5", got)
	}
}

func TestMembersAreSortedByID(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.items.set(&item{ID: "c", Ver: 1, Group: "g"}, &item{ID: "a", Ver: 1, Group: "g"}, &item{ID: "b", Ver: 1, Group: "g"})
	r.e.Register(ctlGroups(r))
	r.load()
	if got := viewB(r.view(), "g"); got == nil || got.S != "a,b,c" {
		t.Fatalf("members = %+v, want a,b,c", got)
	}
}

func TestEmptiedGroupDrops(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.items.set(&item{ID: "i1", Ver: 1, Group: "g1"})
	r.e.Register(ctlGroups(r))
	r.load()

	r.putItem(&item{ID: "i1", Ver: 2, Group: "g2"})
	v := r.view()
	if viewB(v, "g1") != nil {
		t.Fatal("group with no members still published")
	}
	if viewB(v, "g2") == nil {
		t.Fatal("group with a new member not published")
	}
}

func TestReferrersListRecordedReaders(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.parents.set(&parent{ID: "p1", Name: "alpha", Ver: 1})
	r.items.set(
		&item{ID: "i2", Ver: 1, Parent: "p1", ParentName: "alpha"},
		&item{ID: "i1", Ver: 1, Parent: "p1", ParentName: "beta"},
	)
	r.e.Register(ctlByID(r))
	r.e.Register(ctlByName(r))
	r.load()

	v := r.view()
	got := v.Referrers(engine.ByID(kParent, "p1"))
	want := []engine.Ref{refOf(kA, "i1"), refOf(kA, "i2")}
	if !slices.Equal(got, want) {
		t.Fatalf("Referrers(ByID p1) = %v, want %v", got, want)
	}
	if got := v.Referrers(engine.ByName(kParent, "beta")); !slices.Equal(got, []engine.Ref{refOf(kB, "i1")}) {
		t.Fatalf("Referrers(ByName beta) = %v, want [b/i1]", got)
	}
	if got := v.Referrers(engine.Any(kParent)); got != nil {
		t.Fatalf("Referrers(Any) = %v, want nil", got)
	}

	r.putItem(&item{ID: "i1", Ver: 2, Parent: "p9", ParentName: "beta"})
	if got := r.view().Referrers(engine.ByID(kParent, "p1")); !slices.Equal(got, []engine.Ref{refOf(kA, "i2")}) {
		t.Fatalf("Referrers after edge moved = %v, want [a/i2]", got)
	}
}
