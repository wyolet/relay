// Package settingswatch applies a value-typed settings section to a live
// consumer, re-applying whenever the section changes. It is the
// lightweight counterpart to observers like payloadlog's Controller:
// those own sink/emitter lifecycle and rebuild their own resources,
// whereas a Watcher just reads one section and hands the resolved value
// to an apply callback.
//
// Use it for knobs whose runtime effect is "call this setter with the
// new value" (e.g. a parser toggle). The callback is where any
// vendor-specific or impure coupling lives, so the watcher itself stays
// vendor-neutral and testable.
//
// The watcher is event-driven: it applies the current value once at
// Start and then re-applies on each settings-change callback from the
// Source — no polling. Because the callback fires on the catalog's
// serial NOTIFY goroutine, the apply must be cheap (it is, for a
// setter); a consumer with heavy work should use payloadlog's
// signal-a-goroutine pattern instead.
package settingswatch

import (
	"log/slog"
	"sync"
)

// Source is a live settings provider: read a section's current value and
// subscribe to its changes. Satisfied by *app/catalog.Catalog.
type Source interface {
	Setting(section string) (any, bool)
	OnSettingsChange(section string, fn func())
}

// Watcher applies one settings section to a consumer via an apply
// callback, on first Start and on every subsequent change. T is the
// section's value type; it must be comparable so change detection is a
// plain ==.
type Watcher[T comparable] struct {
	src     Source
	section string
	apply   func(T)
	log     *slog.Logger

	mu      sync.Mutex
	applied T
	has     bool
}

// New builds a Watcher for section. apply is called with the resolved
// value at Start and on every change. If apply panics it is recovered
// and logged so a bad value can't take down the caller (or the catalog
// listener goroutine the change callback runs on).
func New[T comparable](src Source, section string, apply func(T), log *slog.Logger) *Watcher[T] {
	if log == nil {
		log = slog.Default()
	}
	return &Watcher[T]{src: src, section: section, apply: apply, log: log}
}

// Start applies the current value once, then subscribes for changes.
// Synchronous and cheap; call it during composition after the catalog is
// wired. Reconcile order (apply-then-subscribe) guarantees at least one
// apply even if the section never changes; the mutex makes a change that
// races the initial apply safe.
func (w *Watcher[T]) Start() {
	w.reconcile()
	w.src.OnSettingsChange(w.section, w.reconcile)
}

// reconcile reads the current value and applies it if it differs from the
// last applied value (or nothing has been applied yet). A missing or
// wrong-typed value is skipped — the consumer keeps its prior state.
func (w *Watcher[T]) reconcile() {
	w.mu.Lock()
	defer w.mu.Unlock()

	v, ok := w.src.Setting(w.section)
	if !ok {
		return
	}
	cur, ok := v.(*T)
	if !ok || cur == nil {
		w.log.Warn("settingswatch: unexpected value type", "section", w.section)
		return
	}
	if w.has && *cur == w.applied {
		return
	}
	w.invoke(*cur)
	w.applied = *cur
	w.has = true
}

func (w *Watcher[T]) invoke(v T) {
	defer func() {
		if r := recover(); r != nil {
			w.log.Error("settingswatch: apply panicked", "section", w.section, "panic", r)
		}
	}()
	w.apply(v)
}
