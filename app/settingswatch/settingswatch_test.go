package settingswatch

import (
	"sync"
	"testing"
)

type knob struct {
	RichParsing bool
}

// fakeSource holds one section's value and the registered change
// callbacks, and lets a test mutate the value and fire the callbacks the
// way the catalog NOTIFY listener would.
type fakeSource struct {
	mu   sync.Mutex
	val  any
	ok   bool
	subs []func()
}

func (f *fakeSource) set(v any, ok bool) {
	f.mu.Lock()
	f.val, f.ok = v, ok
	f.mu.Unlock()
}

// change updates the value and fires callbacks, mimicking applyUpsert →
// notify (state visible before callbacks run).
func (f *fakeSource) change(v any) {
	f.set(v, true)
	f.mu.Lock()
	subs := append([]func(){}, f.subs...)
	f.mu.Unlock()
	for _, fn := range subs {
		fn()
	}
}

func (f *fakeSource) Setting(string) (any, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.val, f.ok
}

func (f *fakeSource) OnSettingsChange(_ string, fn func()) {
	f.mu.Lock()
	f.subs = append(f.subs, fn)
	f.mu.Unlock()
}

func TestStartAppliesOnceThenOnChangeOnly(t *testing.T) {
	s := &fakeSource{}
	s.set(&knob{RichParsing: true}, true)

	var got []knob
	w := New[knob](s, "parsing", func(k knob) { got = append(got, k) }, nil)

	w.Start()                           // initial: applies true
	s.change(&knob{RichParsing: true})  // unchanged: skipped
	s.change(&knob{RichParsing: false}) // changed: applies false
	s.change(&knob{RichParsing: false}) // unchanged: skipped

	if len(got) != 2 {
		t.Fatalf("apply count = %d, want 2 (initial + one change); got %+v", len(got), got)
	}
	if got[0] != (knob{RichParsing: true}) || got[1] != (knob{RichParsing: false}) {
		t.Fatalf("applied values = %+v, want [true,false]", got)
	}
}

func TestStartSubscribesForLaterChanges(t *testing.T) {
	s := &fakeSource{}
	s.set(&knob{RichParsing: true}, true)
	var last knob
	w := New[knob](s, "parsing", func(k knob) { last = k }, nil)
	w.Start()
	s.change(&knob{RichParsing: false})
	if last.RichParsing {
		t.Fatal("change callback did not re-apply: still RichParsing=true")
	}
}

func TestReconcileSkipsMissingOrWrongType(t *testing.T) {
	s := &fakeSource{}
	var calls int
	w := New[knob](s, "parsing", func(knob) { calls++ }, nil)

	s.set(nil, false) // section absent
	w.Start()
	s.set("not-a-knob", true) // wrong type
	w.reconcile()
	if calls != 0 {
		t.Fatalf("apply called %d times on missing/wrong-type, want 0", calls)
	}

	s.set(&knob{RichParsing: true}, true)
	w.reconcile()
	if calls != 1 {
		t.Fatalf("apply called %d times after valid value, want 1", calls)
	}
}

func TestInvokeRecoversPanic(t *testing.T) {
	s := &fakeSource{}
	s.set(&knob{RichParsing: true}, true)
	w := New[knob](s, "parsing", func(knob) { panic("boom") }, nil)
	w.Start() // must not propagate the panic
}
