package catalog

import (
	"context"
	"sync"
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

// OnSettingsChange registers fn to run whenever section's value changes
// (upsert or delete), after the new value is visible to Setting. fn runs
// synchronously on the NOTIFY listener goroutine, which applies catalog
// events serially — so fn MUST be cheap and non-blocking. A consumer
// with heavy work (sink rebuild, network I/O) should have fn do a
// non-blocking signal and perform the work on its own goroutine.
// Subscriptions are process-lifetime; there is no unsubscribe.
func (c *Catalog) OnSettingsChange(section string, fn func()) {
	c.settings.subscribe(section, fn)
}

// settingsHolder lives on Catalog as a sibling to the entity Snapshot.
// Settings have no cross-refs to validate so they don't need the
// reconciler — just an atomic-swap cache fed by the NOTIFY listener.
type settingsHolder struct {
	store SettingsLister
	state atomic.Pointer[settingsState]

	submu sync.RWMutex
	subs  map[string][]func()
}

// subscribe registers fn against section. Caller-facing semantics live on
// Catalog.OnSettingsChange.
func (h *settingsHolder) subscribe(section string, fn func()) {
	h.submu.Lock()
	defer h.submu.Unlock()
	if h.subs == nil {
		h.subs = make(map[string][]func())
	}
	h.subs[section] = append(h.subs[section], fn)
}

// notify runs every callback registered for section. Called after the
// state swap so callbacks observe the new value via loadSection.
func (h *settingsHolder) notify(section string) {
	h.submu.RLock()
	fns := h.subs[section]
	h.submu.RUnlock()
	for _, fn := range fns {
		fn()
	}
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
// new state in. Called on boot (inside Hydrate) and as a fallback
// recovery path. Notifies every subscribed section afterward: reload
// is the boot path, so subscribers wired before Hydrate get their
// first real value here rather than from a per-section NOTIFY. Wiring
// must register subscriptions before Hydrate runs for this to land.
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
	h.notifyAll()
	return nil
}

// notifyAll fires every subscribed section's callbacks. Used after a
// full reload, where individual changed sections aren't tracked.
func (h *settingsHolder) notifyAll() {
	h.submu.RLock()
	sections := make([]string, 0, len(h.subs))
	for s := range h.subs {
		sections = append(sections, s)
	}
	h.submu.RUnlock()
	for _, s := range sections {
		h.notify(s)
	}
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
	h.notify(section)
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
	h.notify(section)
}
