package catalog

import (
	"context"
	"sync/atomic"

	"github.com/wyolet/relay/app/settings"
)

// SettingsLister is the narrow read interface the catalog needs to
// bootstrap and refresh the settings cache. *settings.Store satisfies
// it.
type SettingsLister interface {
	List(ctx context.Context) ([]*settings.Row, error)
	Get(ctx context.Context, section string) (*settings.Row, error)
}

// settingsState is the live in-memory view of all known settings
// sections, keyed by section name. Replaced atomically on any change
// so request-path readers never see a torn map.
type settingsState struct {
	m map[string]any
}

// Setting returns the typed value for section, falling back to the
// section's Defaults() when no row has been seen yet. ok=false means
// the section name is not registered.
func (c *Catalog) Setting(section string) (any, bool) {
	return c.settings.loadSection(section)
}

// settingsHolder lives on Catalog as a sibling to the entity Snapshot.
// Settings have no cross-refs to validate so they don't need the
// reconciler — just an atomic-swap cache fed by the NOTIFY listener.
type settingsHolder struct {
	store SettingsLister
	state atomic.Pointer[settingsState]
}

func (h *settingsHolder) load() map[string]any {
	s := h.state.Load()
	if s == nil {
		return nil
	}
	return s.m
}

// loadSection returns the typed value for section, falling back to the
// section's Defaults() when no row has been seen. ok=false means the
// section is unregistered — callers should treat as 404.
func (h *settingsHolder) loadSection(name string) (any, bool) {
	sec, ok := settings.Lookup(name)
	if !ok {
		return nil, false
	}
	if v, present := h.load()[name]; present {
		return v, true
	}
	return sec.Defaults(), true
}

// reload re-reads every section from the store and atomic-swaps the
// new state in. Called on boot and as a fallback recovery path.
func (h *settingsHolder) reload(ctx context.Context) error {
	rows, err := h.store.List(ctx)
	if err != nil {
		return err
	}
	next := &settingsState{m: make(map[string]any, len(rows))}
	for _, r := range rows {
		next.m[r.Section] = r.Value
	}
	h.state.Store(next)
	return nil
}

// applyUpsert re-fetches one section via the store and merges it into
// a fresh state copy. The COW pattern keeps the request path lock-free.
func (h *settingsHolder) applyUpsert(ctx context.Context, section string) error {
	row, err := h.store.Get(ctx, section)
	if err != nil {
		return err
	}
	prev := h.load()
	next := &settingsState{m: make(map[string]any, len(prev)+1)}
	for k, v := range prev {
		next.m[k] = v
	}
	next.m[section] = row.Value
	h.state.Store(next)
	return nil
}

// applyDelete drops a section from the cache. Readers fall back to
// Defaults().
func (h *settingsHolder) applyDelete(section string) {
	prev := h.load()
	next := &settingsState{m: make(map[string]any, len(prev))}
	for k, v := range prev {
		if k == section {
			continue
		}
		next.m[k] = v
	}
	h.state.Store(next)
}
