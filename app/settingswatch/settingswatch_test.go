package settingswatch

import (
	"testing"

	"github.com/wyolet/relay/app/settings/settingstest"
)

type knob struct {
	RichParsing bool
}

const section = "parsing"

func TestStartAppliesOnceThenOnChangeOnly(t *testing.T) {
	s := &settingstest.Source{}
	s.Set(section, &knob{RichParsing: true})

	var got []knob
	w := New[knob](s, section, func(k knob) { got = append(got, k) }, nil)

	w.Start()                                    // initial: applies true
	s.Change(section, &knob{RichParsing: true})  // unchanged: skipped
	s.Change(section, &knob{RichParsing: false}) // changed: applies false
	s.Change(section, &knob{RichParsing: false}) // unchanged: skipped

	if len(got) != 2 {
		t.Fatalf("apply count = %d, want 2 (initial + one change); got %+v", len(got), got)
	}
	if got[0] != (knob{RichParsing: true}) || got[1] != (knob{RichParsing: false}) {
		t.Fatalf("applied values = %+v, want [true,false]", got)
	}
}

func TestStartSubscribesForLaterChanges(t *testing.T) {
	s := &settingstest.Source{}
	s.Set(section, &knob{RichParsing: true})
	var last knob
	w := New[knob](s, section, func(k knob) { last = k }, nil)
	w.Start()
	s.Change(section, &knob{RichParsing: false})
	if last.RichParsing {
		t.Fatal("change callback did not re-apply: still RichParsing=true")
	}
}

func TestReconcileSkipsMissingOrWrongType(t *testing.T) {
	s := &settingstest.Source{}
	var calls int
	w := New[knob](s, section, func(knob) { calls++ }, nil)

	s.Delete(section) // section absent
	w.Start()
	s.Set(section, "not-a-knob") // wrong type
	w.reconcile()
	if calls != 0 {
		t.Fatalf("apply called %d times on missing/wrong-type, want 0", calls)
	}

	s.Set(section, &knob{RichParsing: true})
	w.reconcile()
	if calls != 1 {
		t.Fatalf("apply called %d times after valid value, want 1", calls)
	}
}

func TestInvokeRecoversPanic(t *testing.T) {
	s := &settingstest.Source{}
	s.Set(section, &knob{RichParsing: true})
	w := New[knob](s, section, func(knob) { panic("boom") }, nil)
	w.Start() // must not propagate the panic
}
