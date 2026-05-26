package settingswatch

import (
	"context"
	"sync"
	"testing"
	"time"
)

type knob struct {
	RichParsing bool
}

// fakeReader returns whatever value is set under its lock.
type fakeReader struct {
	mu  sync.Mutex
	val any
	ok  bool
}

func (f *fakeReader) set(v any, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.val, f.ok = v, ok
}

func (f *fakeReader) Setting(string) (any, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.val, f.ok
}

func TestReconcileAppliesInitialThenOnChangeOnly(t *testing.T) {
	r := &fakeReader{}
	r.set(&knob{RichParsing: true}, true)

	var got []knob
	w := New[knob](r, "parsing", func(k knob) { got = append(got, k) }, nil)

	w.reconcile() // first: applies
	w.reconcile() // unchanged: skipped
	r.set(&knob{RichParsing: false}, true)
	w.reconcile() // changed: applies
	w.reconcile() // unchanged: skipped

	if len(got) != 2 {
		t.Fatalf("apply count = %d, want 2 (initial + one change); got %+v", len(got), got)
	}
	if got[0] != (knob{RichParsing: true}) || got[1] != (knob{RichParsing: false}) {
		t.Fatalf("applied values = %+v, want [true,false]", got)
	}
}

func TestReconcileSkipsMissingOrWrongType(t *testing.T) {
	r := &fakeReader{}
	var calls int
	w := New[knob](r, "parsing", func(knob) { calls++ }, nil)

	r.set(nil, false) // section absent
	w.reconcile()
	r.set("not-a-knob", true) // wrong type
	w.reconcile()
	if calls != 0 {
		t.Fatalf("apply called %d times on missing/wrong-type, want 0", calls)
	}

	r.set(&knob{RichParsing: true}, true)
	w.reconcile()
	if calls != 1 {
		t.Fatalf("apply called %d times after valid value, want 1", calls)
	}
}

func TestInvokeRecoversPanic(t *testing.T) {
	r := &fakeReader{}
	r.set(&knob{RichParsing: true}, true)
	w := New[knob](r, "parsing", func(knob) { panic("boom") }, nil)
	w.reconcile() // must not propagate the panic
}

func TestRunStopsOnContextCancel(t *testing.T) {
	r := &fakeReader{}
	r.set(&knob{RichParsing: true}, true)
	w := New[knob](r, "parsing", func(knob) {}, nil)
	w.interval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}
