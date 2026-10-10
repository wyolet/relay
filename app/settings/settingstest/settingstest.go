// Package settingstest serves settings sections from memory for tests. Source
// satisfies app/settings.Reader and app/settingswatch.Source without importing
// either, so tests inside app/settings can use it too. It does not decode or
// validate: a test stores the typed value a consumer expects to read.
package settingstest

import "sync"

// Source holds the sections a test stores and the change callbacks consumers
// register per section. The zero value is empty and ready to use.
type Source struct {
	mu     sync.Mutex
	values map[string]any
	subs   map[string][]func()
}

// Sections returns a Source holding values, keyed by section name.
func Sections(values map[string]any) *Source {
	s := &Source{}
	for section, v := range values {
		s.Set(section, v)
	}
	return s
}

func (s *Source) Setting(section string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[section]
	return v, ok
}

func (s *Source) OnSettingsChange(section string, fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.subs == nil {
		s.subs = map[string][]func(){}
	}
	s.subs[section] = append(s.subs[section], fn)
}

// Callbacks reports how many change callbacks are registered for section.
func (s *Source) Callbacks(section string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs[section])
}

// Set stores v under section without running its callbacks.
func (s *Source) Set(section string, v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = map[string]any{}
	}
	s.values[section] = v
}

// Delete removes section, so it reads as absent.
func (s *Source) Delete(section string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, section)
}

// Change stores v under section, then runs the section's callbacks outside
// the lock, the order the catalog's settings listener keeps: the new value
// is readable before any callback runs.
func (s *Source) Change(section string, v any) {
	s.Set(section, v)
	s.mu.Lock()
	subs := append([]func(){}, s.subs[section]...)
	s.mu.Unlock()
	for _, fn := range subs {
		fn()
	}
}
