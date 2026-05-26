// Package settingswatch applies a value-typed settings section to a live
// consumer, hot-swapping on change. It is the lightweight counterpart to
// observers like payloadlog's Controller: those own sink/emitter
// lifecycle and reconcile their own resources, whereas a Watcher just
// reads one section and hands the resolved value to an apply callback.
//
// Use it for knobs whose runtime effect is "call this setter with the
// new value" (e.g. a parser toggle). The callback is where any
// vendor-specific or impure coupling lives, so the watcher itself stays
// vendor-neutral and testable.
//
// Convergence after a settings PUT is bounded by the poll interval plus
// the catalog NOTIFY debounce (~1s). When the deferred event-driven
// settings change-callback lands, this poll can be replaced by a
// subscription without touching consumers.
package settingswatch

import (
	"context"
	"log/slog"
	"time"
)

// DefaultInterval matches payloadlog's reconcile cadence so settings
// convergence is uniform across consumers.
const DefaultInterval = 2 * time.Second

// Reader reads a live settings section. Satisfied by *app/catalog.Catalog
// (Setting(section) (any, bool)).
type Reader interface {
	Setting(section string) (any, bool)
}

// Watcher polls one settings section and invokes apply whenever the
// resolved value changes (including once on first run). T is the
// section's value type; it must be comparable so change detection is a
// plain ==.
type Watcher[T comparable] struct {
	reader   Reader
	section  string
	apply    func(T)
	log      *slog.Logger
	interval time.Duration

	applied T
	has     bool
}

// New builds a Watcher for section. apply is called with the resolved
// value on the first reconcile and on every subsequent change. apply
// must not panic on bad input — but if it does, the panic is recovered
// and logged so the loop survives.
func New[T comparable](reader Reader, section string, apply func(T), log *slog.Logger) *Watcher[T] {
	if log == nil {
		log = slog.Default()
	}
	return &Watcher[T]{
		reader:   reader,
		section:  section,
		apply:    apply,
		log:      log,
		interval: DefaultInterval,
	}
}

// Run drives the reconcile loop until ctx is cancelled. Call in a
// goroutine after the catalog is wired.
func (w *Watcher[T]) Run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	w.reconcile()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.reconcile()
		}
	}
}

// reconcile reads the current value and applies it if it differs from the
// last applied value (or nothing has been applied yet). A missing or
// wrong-typed section value is skipped — the consumer keeps its prior
// state and the next tick retries.
func (w *Watcher[T]) reconcile() {
	v, ok := w.reader.Setting(w.section)
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
