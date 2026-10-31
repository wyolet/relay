package engine_test

import (
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/engine"
)

// chain registers a → b → c: a copies the item's parent value, b reads a, c reads b; each adds one.
func chain(r *rig, reversed bool) {
	a := engine.NewController("ctl-a", kA, func(tx *engine.Txn, id string) error {
		r.calls.hit("ctl-a", id)
		it := engine.Get[item](tx, id)
		if it == nil {
			engine.Drop(tx)
			return nil
		}
		p := engine.Get[parent](tx, it.Parent)
		if p == nil {
			engine.Drop(tx)
			return nil
		}
		engine.Put(tx, &outA{V: p.Val})
		return nil
	})
	b := engine.NewController("ctl-b", kB, func(tx *engine.Txn, id string) error {
		r.calls.hit("ctl-b", id)
		up := engine.Get[outA](tx, id)
		if up == nil {
			engine.Drop(tx)
			return nil
		}
		engine.Put(tx, &outB{V: up.V + 1})
		return nil
	})
	c := engine.NewController("ctl-c", kC, func(tx *engine.Txn, id string) error {
		r.calls.hit("ctl-c", id)
		up := engine.Get[outB](tx, id)
		if up == nil {
			engine.Drop(tx)
			return nil
		}
		engine.Put(tx, &outC{V: up.V + 1})
		return nil
	})
	for _, ctl := range []*engine.Controller{a, b, c} {
		engine.Watch(ctl, selfKey)
	}
	order := []*engine.Controller{a, b, c}
	if reversed {
		slices.Reverse(order)
	}
	for _, ctl := range order {
		r.e.Register(ctl)
	}
}

func TestDerivedChainReachesFixpointInOneGeneration(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.parents.set(&parent{ID: "p1", Ver: 1, Val: 10})
	r.items.set(&item{ID: "i1", Ver: 1, Parent: "p1"})
	chain(r, false)
	r.load()
	if got := viewC(r.view(), "i1"); got == nil || got.V != 12 {
		t.Fatalf("c after Load = %+v, want V=12", got)
	}

	r.putParent(&parent{ID: "p1", Ver: 2, Val: 20})
	s := r.nextPub()
	if s.Gen != 2 {
		t.Fatalf("one write published up to gen %d, want 2", s.Gen)
	}
	if got := viewC(r.view(), "i1"); got == nil || got.V != 22 {
		t.Fatalf("c after write = %+v, want V=22", got)
	}
	r.expectNoPub(50 * time.Millisecond)
}

func TestChildRegisteredBeforeParentStillConverges(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.parents.set(&parent{ID: "p1", Ver: 1, Val: 10})
	r.items.set(&item{ID: "i1", Ver: 1, Parent: "p1"})
	chain(r, true)
	r.load()
	if got := viewC(r.view(), "i1"); got == nil || got.V != 12 {
		t.Fatalf("c after Load = %+v, want V=12", got)
	}
	r.putParent(&parent{ID: "p1", Ver: 2, Val: 20})
	s := r.nextPub()
	if got := viewC(r.view(), "i1"); got == nil || got.V != 22 {
		t.Fatalf("c after write = %+v, want V=22", got)
	}
	if s.Rounds < 2 {
		t.Fatalf("rounds = %d, want more than one when children run first", s.Rounds)
	}
}

func TestUnchangedPutStopsPropagation(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.parents.set(&parent{ID: "p1", Name: "x", Ver: 1, Val: 1})
	r.items.set(&item{ID: "i1", Ver: 1, Parent: "p1"})
	a := engine.NewController("same-or", kA, func(tx *engine.Txn, id string) error {
		r.calls.hit("same-or", id)
		p := engine.Get[parent](tx, engine.Get[item](tx, id).Parent)
		if prev := engine.Get[outA](tx, id); prev != nil && prev.V == p.Val {
			engine.Put(tx, prev)
			return nil
		}
		engine.Put(tx, &outA{V: p.Val})
		return nil
	})
	b := engine.NewController("reader", kB, func(tx *engine.Txn, id string) error {
		r.calls.hit("reader", id)
		engine.Put(tx, &outB{V: engine.Get[outA](tx, id).V})
		return nil
	})
	engine.Watch(a, selfKey)
	engine.Watch(b, selfKey)
	r.e.Register(a)
	r.e.Register(b)
	r.load()
	before := viewA(r.view(), "i1")

	r.putParent(&parent{ID: "p1", Name: "y", Ver: 2, Val: 1})
	if r.calls.count("same-or", "i1") == 0 {
		t.Fatal("parent write did not reconcile its reader")
	}
	if n := r.calls.count("reader", "i1"); n != 0 {
		t.Fatalf("unchanged output propagated: downstream reconciled %d times", n)
	}
	if viewA(r.view(), "i1") != before {
		t.Fatal("unchanged output was replaced")
	}

	r.putParent(&parent{ID: "p1", Name: "y", Ver: 3, Val: 2})
	if n := r.calls.count("reader", "i1"); n != 1 {
		t.Fatalf("changed output reconciled downstream %d times, want 1", n)
	}
}

func TestCycleIsAbandonedAndReported(t *testing.T) {
	abandoned := make(chan int, 16)
	r := newRig(t, engine.Options{MaxRounds: 4, Hooks: engine.Hooks{
		OnBatchAbandoned: func(_ engine.Generation, rounds int) {
			select {
			case abandoned <- rounds:
			default:
			}
		},
	}})
	r.items.set(&item{ID: "i1", Ver: 1})
	// While on, each side builds a new object from the other's, so the pair never settles.
	var on atomic.Bool
	a := engine.NewController("ping", kA, func(tx *engine.Txn, id string) error {
		b := engine.Get[outB](tx, id)
		if prev := engine.Get[outA](tx, id); prev != nil && !on.Load() {
			engine.Put(tx, prev)
			return nil
		}
		v := 0
		if b != nil {
			v = b.V + 1
		}
		engine.Put(tx, &outA{V: v})
		return nil
	})
	b := engine.NewController("pong", kB, func(tx *engine.Txn, id string) error {
		a := engine.Get[outA](tx, id)
		if prev := engine.Get[outB](tx, id); prev != nil && !on.Load() {
			engine.Put(tx, prev)
			return nil
		}
		v := 0
		if a != nil {
			v = a.V + 1
		}
		engine.Put(tx, &outB{V: v})
		return nil
	})
	engine.Watch(a, selfKey)
	engine.Watch(b, selfKey)
	r.e.Register(a)
	r.e.Register(b)
	r.load()

	on.Store(true)
	r.putItem(&item{ID: "i1", Ver: 2})
	select {
	case n := <-abandoned:
		if n > 4 {
			t.Fatalf("abandoned after %d rounds, want at most MaxRounds=4", n)
		}
	default:
		t.Fatal("cycle did not call OnBatchAbandoned before Apply returned")
	}
	if !slices.Contains(issueCodes(r.view().AllIssues()), "batch_abandoned") {
		t.Fatalf("issues = %v, want batch_abandoned", issueCodes(r.view().AllIssues()))
	}
	on.Store(false)
}

var errReconcile = errors.New("upstream table unreadable")

type errorLog struct {
	mu   sync.Mutex
	outs []engine.Ref
	ctls []string
	errs []error
}

func (l *errorLog) record(ctl string, out engine.Ref, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ctls = append(l.ctls, ctl)
	l.outs = append(l.outs, out)
	l.errs = append(l.errs, err)
}

func (l *errorLog) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.outs)
}

// ctlFailing copies the parent value into outA, failing with fail() for the ids it names.
func ctlFailing(r *rig, fail func(id string) error) *engine.Controller {
	c := engine.NewController("failing", kA, func(tx *engine.Txn, id string) error {
		r.calls.hit("failing", id)
		it := engine.Get[item](tx, id)
		if it == nil {
			engine.Drop(tx)
			return nil
		}
		p := engine.Get[parent](tx, it.Parent)
		if err := fail(id); err != nil {
			return err
		}
		if p == nil {
			engine.Drop(tx)
			return nil
		}
		engine.Put(tx, &outA{V: p.Val})
		return nil
	})
	engine.Watch(c, selfKey)
	return c
}

func TestReconcileErrorKeepsPreviousOutputAndRaisesIssue(t *testing.T) {
	log := &errorLog{}
	r := newRig(t, engine.Options{Hooks: engine.Hooks{OnReconcileError: log.record}})
	r.parents.set(&parent{ID: "p1", Ver: 1, Val: 1})
	r.items.set(&item{ID: "i1", Ver: 1, Parent: "p1"})
	var failing atomic.Bool
	r.e.Register(ctlFailing(r, func(string) error {
		if failing.Load() {
			return errReconcile
		}
		return nil
	}))
	r.load()
	before := viewA(r.view(), "i1")

	failing.Store(true)
	_ = engine.Apply(r.e, &parent{ID: "p1", Ver: 2, Val: 2})
	r.parents.set(&parent{ID: "p1", Ver: 2, Val: 2})
	v := r.view()
	if viewA(v, "i1") != before {
		t.Fatalf("output after failed reconcile = %+v, want the previous one", viewA(v, "i1"))
	}
	is := v.Issues(refOf(kA, "i1"))
	if len(is) != 1 || is[0].Code != "reconcile_failed" || is[0].Severity != engine.SeverityError {
		t.Fatalf("issues = %+v, want one reconcile_failed error", is)
	}
	eventually(t, "OnReconcileError", func() bool { return log.len() > 0 })
	log.mu.Lock()
	if log.ctls[0] != "failing" || log.outs[0] != refOf(kA, "i1") || !errors.Is(log.errs[0], errReconcile) {
		t.Errorf("OnReconcileError(%q, %v, %v), want (failing, a/i1, errReconcile)", log.ctls[0], log.outs[0], log.errs[0])
	}
	log.mu.Unlock()

	failing.Store(false)
	r.putParent(&parent{ID: "p1", Ver: 3, Val: 3})
	eventually(t, "recovered output", func() bool {
		o := viewA(r.view(), "i1")
		return o != nil && o.V == 3 && len(r.view().Issues(refOf(kA, "i1"))) == 0
	})
}

func TestPanicInReconcileIsTreatedAsErrorAndOthersPublish(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.parents.set(&parent{ID: "p1", Ver: 1, Val: 1})
	r.items.set(&item{ID: "i1", Ver: 1, Parent: "p1"}, &item{ID: "i2", Ver: 1, Parent: "p1"})
	var boom atomic.Bool
	r.e.Register(ctlFailing(r, func(id string) error {
		if boom.Load() && id == "i1" {
			panic("reconcile bug")
		}
		return nil
	}))
	r.load()
	before := viewA(r.view(), "i1")

	boom.Store(true)
	r.parents.set(&parent{ID: "p1", Ver: 2, Val: 2})
	_ = engine.Apply(r.e, &parent{ID: "p1", Ver: 2, Val: 2})
	boom.Store(false)
	v := r.view()
	if viewA(v, "i1") != before {
		t.Fatal("panicking reconcile replaced its previous output")
	}
	if !slices.Contains(issueCodes(v.Issues(refOf(kA, "i1"))), "reconcile_failed") {
		t.Fatalf("issues on panicking output = %v, want reconcile_failed", issueCodes(v.Issues(refOf(kA, "i1"))))
	}
	if got := viewA(v, "i2"); got == nil || got.V != 2 {
		t.Fatalf("sibling output = %+v, want V=2 published despite the panic", got)
	}
}

func TestSecondPutInOneReconcileFailsIt(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.items.set(&item{ID: "i1", Ver: 1})
	var twice atomic.Bool
	c := engine.NewController("double", kA, func(tx *engine.Txn, id string) error {
		it := engine.Get[item](tx, id)
		engine.Put(tx, &outA{V: it.Val})
		if twice.Load() {
			engine.Put(tx, &outA{V: it.Val})
		}
		return nil
	})
	engine.Watch(c, selfKey)
	r.e.Register(c)
	r.load()
	before := viewA(r.view(), "i1")

	twice.Store(true)
	r.items.set(&item{ID: "i1", Ver: 2, Val: 2})
	_ = engine.Apply(r.e, &item{ID: "i1", Ver: 2, Val: 2})
	twice.Store(false)
	v := r.view()
	if viewA(v, "i1") != before {
		t.Fatal("a reconcile that called Put twice replaced its output")
	}
	if !slices.Contains(issueCodes(v.Issues(refOf(kA, "i1"))), "reconcile_failed") {
		t.Fatalf("issues = %v, want reconcile_failed", issueCodes(v.Issues(refOf(kA, "i1"))))
	}
}

func TestErrRetryIsRetriedOnTheNextDrain(t *testing.T) {
	t.Skip("contract mismatch: an ErrRetry output is not retried in the next Apply drain, only by a later drain of its own")
	r := newRig(t, engine.Options{})
	r.parents.set(&parent{ID: "p1", Ver: 1, Val: 1}, &parent{ID: "p9", Ver: 1})
	r.items.set(&item{ID: "i1", Ver: 1, Parent: "p1"})
	var transient atomic.Bool
	r.e.Register(ctlFailing(r, func(string) error {
		if transient.Load() {
			return engine.ErrRetry
		}
		return nil
	}))
	r.load()
	before := viewA(r.view(), "i1")

	transient.Store(true)
	r.parents.set(&parent{ID: "p1", Ver: 2, Val: 2})
	_ = engine.Apply(r.e, &parent{ID: "p1", Ver: 2, Val: 2})
	if viewA(r.view(), "i1") != before {
		t.Fatal("ErrRetry replaced the previous output")
	}

	transient.Store(false)
	r.putParent(&parent{ID: "p9", Ver: 2})
	if got := viewA(r.view(), "i1"); got == nil || got.V != 2 {
		t.Fatalf("output after the next drain = %+v, want V=2", got)
	}
}

func TestErrRetryRecoversWithoutAnotherWrite(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.parents.set(&parent{ID: "p1", Ver: 1, Val: 1})
	r.items.set(&item{ID: "i1", Ver: 1, Parent: "p1"})
	var transient atomic.Bool
	r.e.Register(ctlFailing(r, func(string) error {
		if transient.Load() {
			return engine.ErrRetry
		}
		return nil
	}))
	r.load()

	transient.Store(true)
	r.parents.set(&parent{ID: "p1", Ver: 2, Val: 2})
	_ = engine.Apply(r.e, &parent{ID: "p1", Ver: 2, Val: 2})
	transient.Store(false)
	eventually(t, "retried output", func() bool {
		o := viewA(r.view(), "i1")
		return o != nil && o.V == 2 && len(r.view().Issues(refOf(kA, "i1"))) == 0
	})
}

// ctlNoted raises the item's Note as an issue and an extra warning when Val is negative.
func ctlNoted(r *rig) *engine.Controller {
	c := engine.NewController("noted", kA, func(tx *engine.Txn, id string) error {
		it := engine.Get[item](tx, id)
		if it == nil {
			engine.Drop(tx)
			return nil
		}
		if it.Note != "" {
			tx.Issue(engine.Issue{Severity: engine.SeverityError, Code: it.Note})
		}
		if it.Val < 0 {
			tx.Issue(engine.Issue{Severity: engine.SeverityWarning, Code: "a_warning"})
			tx.Issue(engine.Issue{Severity: engine.SeverityError, Code: "b_error"})
		}
		engine.Put(tx, &outA{V: it.Val})
		return nil
	})
	engine.Watch(c, selfKey)
	return c
}

func TestIssuesAreReplacedOnEachReconcile(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.items.set(&item{ID: "i1", Ver: 1, Note: "first_problem"})
	r.e.Register(ctlNoted(r))
	r.load()
	out := refOf(kA, "i1")
	if got := issueCodes(r.view().Issues(out)); !slices.Equal(got, []string{"first_problem"}) {
		t.Fatalf("issues = %v, want [first_problem]", got)
	}

	r.putItem(&item{ID: "i1", Ver: 2, Note: "second_problem"})
	if got := issueCodes(r.view().Issues(out)); !slices.Equal(got, []string{"second_problem"}) {
		t.Fatalf("issues = %v, want only [second_problem]", got)
	}

	r.putItem(&item{ID: "i1", Ver: 3})
	if got := r.view().Issues(out); len(got) != 0 {
		t.Fatalf("issues after clean reconcile = %v, want none", issueCodes(got))
	}
	if got := r.view().AllIssues(); len(got) != 0 {
		t.Fatalf("AllIssues after clean reconcile = %v, want none", issueCodes(got))
	}
}

func TestIssueSourceDefaultsToTheOutput(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.items.set(&item{ID: "i1", Ver: 1, Note: "some_problem"})
	r.e.Register(ctlNoted(r))
	r.load()
	is := r.view().Issues(refOf(kA, "i1"))
	if len(is) != 1 || is[0].Source != refOf(kA, "i1") {
		t.Fatalf("issues = %+v, want one with Source a/i1", is)
	}
}

func TestTxnOutNamesTheOutputBeingBuilt(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.items.set(&item{ID: "i1", Ver: 1})
	var mu sync.Mutex
	var seen []engine.Ref
	c := engine.NewController("out", kA, func(tx *engine.Txn, id string) error {
		mu.Lock()
		seen = append(seen, tx.Out())
		mu.Unlock()
		engine.Put(tx, &outA{})
		return nil
	})
	engine.Watch(c, selfKey)
	r.e.Register(c)
	r.load()
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 || seen[0] != refOf(kA, "i1") {
		t.Fatalf("Out() = %v, want a/i1", seen)
	}
}

func TestIssuesSortBySeverityThenCode(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.items.set(&item{ID: "i1", Ver: 1, Val: -1, Note: "c_error"})
	r.e.Register(ctlNoted(r))
	r.load()
	got := issueCodes(r.view().Issues(refOf(kA, "i1")))
	if want := []string{"b_error", "c_error", "a_warning"}; !slices.Equal(got, want) {
		t.Fatalf("issue order = %v, want %v", got, want)
	}
}

func TestIssuesOfAnOldViewDoNotChange(t *testing.T) {
	r := newRig(t, engine.Options{})
	r.items.set(&item{ID: "i1", Ver: 1, Note: "old_problem"})
	r.e.Register(ctlNoted(r))
	r.load()
	old := r.view()
	r.putItem(&item{ID: "i1", Ver: 2})
	if got := issueCodes(old.Issues(refOf(kA, "i1"))); !slices.Equal(got, []string{"old_problem"}) {
		t.Fatalf("old view issues = %v, want [old_problem]", got)
	}
}
