package settingswatch

import (
	"slices"
	"testing"

	"github.com/wyolet/relay/app/settings/settingstest"
)

type knob struct{ RichParsing bool }

const section = "parsing"

// The value is applied once at Start and then only when it changes; a section that is absent, of another type or a nil pointer applies nothing, and a change of another section is not this watcher's.
func TestWatcherAppliesOnStartAndOnChange(t *testing.T) {
	on, off := &knob{RichParsing: true}, &knob{RichParsing: false}
	var none *knob
	for _, tc := range []struct {
		name    string
		initial any
		changes []any
		want    []knob
	}{
		{"initial value, then changes", on, []any{on, off, off, on}, []knob{*on, *off, *on}},
		{"an equal value behind another pointer", on, []any{&knob{RichParsing: true}}, []knob{*on}},
		{"absent at start, set later", nil, []any{off}, []knob{*off}},
		{"wrong type, then a valid value", "not-a-knob", []any{knob{RichParsing: true}, none, on}, []knob{*on}},
	} {
		s := &settingstest.Source{}
		if tc.initial != nil {
			s.Set(section, tc.initial)
		}
		var got []knob
		New[knob](s, section, func(k knob) { got = append(got, k) }, nil).Start()
		for _, v := range tc.changes {
			s.Change("another-section", &knob{RichParsing: true})
			s.Change(section, v)
		}
		if !slices.Equal(got, tc.want) || s.Callbacks(section) != 1 || s.Callbacks("another-section") != 0 {
			t.Errorf("%s: applied %+v, want %+v; %d callbacks on the section, want 1", tc.name, got, tc.want, s.Callbacks(section))
		}
	}
}

// A panicking apply does not take the settings goroutine down, and the watcher keeps applying later changes.
func TestApplyPanicIsContained(t *testing.T) {
	s := settingstest.Sections(map[string]any{section: &knob{RichParsing: true}})
	var got []knob
	New[knob](s, section, func(k knob) {
		got = append(got, k)
		if k.RichParsing {
			panic("boom")
		}
	}, nil).Start()
	s.Change(section, &knob{RichParsing: false})
	if want := []knob{{RichParsing: true}, {RichParsing: false}}; !slices.Equal(got, want) {
		t.Errorf("applied %+v, want %+v", got, want)
	}
}
