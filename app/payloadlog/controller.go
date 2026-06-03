package payloadlog

import (
	"context"
	"log/slog"
	"sync"

	"github.com/wyolet/relay/app/settings"
)

// SettingsSource reads a live settings section and subscribes to its
// changes. Satisfied by *app/catalog.Catalog.
type SettingsSource interface {
	Setting(section string) (any, bool)
	OnSettingsChange(section string, fn func())
}

// SinkBuilder constructs the concrete Sink for a resolved config. Injected
// by the composition root so the s3 driver stays behind a build tag — a
// minimal build supplies a builder that errors on backend "s3".
type SinkBuilder func(ctx context.Context, cfg settings.PayloadLogging) (Sink, error)

// Controller owns payload logging's runtime state: a single long-lived
// Emitter draining into a reloadable sink, plus the live enabled/maxBytes
// the Hook and StreamObserver read per request. It reconciles the sink
// (toggle / backend / bucket / credentials) on settings changes without a
// restart — the observer is always registered, so flipping the setting on
// just installs a sink. Changes are event-driven: Subscribe registers a
// catalog change-callback that signals Run; the rebuild (which may do
// network I/O) happens on Run's goroutine, never on the catalog listener.
type Controller struct {
	src     SettingsSource
	build   SinkBuilder
	emitter *Emitter
	rsink   *reloadableSink
	log     *slog.Logger
	kick    chan struct{}

	mu         sync.RWMutex
	enabled    bool
	maxBytes   int
	applied    settings.PayloadLogging
	hasApplied bool
}

// NewController wires the emitter → reloadable-sink chain. The sink starts
// empty (nothing written) until the first reconcile applies the live
// config. reader provides the live settings; build constructs sinks.
func NewController(src SettingsSource, build SinkBuilder, log *slog.Logger) *Controller {
	if log == nil {
		log = slog.Default()
	}
	rsink := &reloadableSink{}
	return &Controller{
		src:     src,
		build:   build,
		emitter: NewEmitter(EmitterOptions{Logger: log}, rsink),
		rsink:   rsink,
		log:     log,
		kick:    make(chan struct{}, 1),
	}
}

// Subscribe registers the controller for payload-logging settings
// changes. Call it synchronously during composition, before the catalog
// Hydrate runs, so the boot reload's notification isn't missed. The
// callback only signals Run; it never rebuilds inline, keeping slow sink
// I/O off the catalog's serial listener goroutine.
func (c *Controller) Subscribe() {
	c.src.OnSettingsChange(settings.SectionPayloadLogging, c.signal)
}

// signal nudges Run to reconcile. Non-blocking: a pending kick already
// means "re-read", so a coalesced extra change is a no-op.
func (c *Controller) signal() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// Emitter is the bounded queue the SinkCollector emits into.
func (c *Controller) Emitter() *Emitter { return c.emitter }

// Enabled reports the live master switch. The Hook/StreamObserver gate on
// this AND the per-request opt-in (lc.PayloadLog).
func (c *Controller) Enabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.enabled
}

// MaxBytes is the live per-body storage cap (0 = unlimited).
func (c *Controller) MaxBytes() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.maxBytes
}

// Run reconciles once, then on every signal from Subscribe's callback,
// until ctx is cancelled. Call in a goroutine after Subscribe. The
// rebuild runs here, off the catalog listener.
func (c *Controller) Run(ctx context.Context) {
	c.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.kick:
			c.reconcile(ctx)
		}
	}
}

// Close drains the emitter and closes the current sink. Idempotent via the
// emitter's own guard.
func (c *Controller) Close() { c.emitter.Close() }

// reconcile reads the live config and, when it differs from what's applied,
// rebuilds (or tears down) the sink and updates the live gate. On a build
// error the previous sink is kept and applied is left unchanged so the next
// tick retries.
func (c *Controller) reconcile(ctx context.Context) {
	cfg := c.current()
	c.mu.RLock()
	applied, has := c.applied, c.hasApplied
	c.mu.RUnlock()
	if has && cfg == applied {
		return
	}

	if !cfg.Enabled {
		c.rsink.swap(nil, c.log)
		c.set(false, cfg.MaxBytes, cfg)
		if has {
			c.log.Debug("payloadlog: disabled via settings")
		}
		return
	}

	sink, err := c.build(ctx, cfg)
	if err != nil {
		c.log.Error("payloadlog: sink build failed; keeping previous sink",
			"err", err, "backend", cfg.Backend)
		return // leave applied unchanged → retry next tick
	}
	c.rsink.swap(sink, c.log)
	c.set(true, cfg.MaxBytes, cfg)
	c.log.Debug("payloadlog: reconciled", "backend", cfg.Backend, "max_bytes", cfg.MaxBytes)
}

func (c *Controller) current() settings.PayloadLogging {
	v, ok := c.src.Setting(settings.SectionPayloadLogging)
	if !ok {
		return settings.PayloadLogging{}
	}
	p, ok := v.(*settings.PayloadLogging)
	if !ok || p == nil {
		return settings.PayloadLogging{}
	}
	return *p
}

func (c *Controller) set(enabled bool, maxBytes int, applied settings.PayloadLogging) {
	c.mu.Lock()
	c.enabled = enabled
	c.maxBytes = maxBytes
	c.applied = applied
	c.hasApplied = true
	c.mu.Unlock()
}

// reloadableSink is the Emitter's sink: it delegates to a swappable inner
// sink under an RWMutex. Write holds the read lock for its full duration,
// so swap (write lock) waits for any in-flight write before replacing and
// closing the old sink — no write-after-close race despite the swap coming
// from a different goroutine than the emitter's drain.
type reloadableSink struct {
	mu  sync.RWMutex
	cur Sink
}

func (r *reloadableSink) Write(rec Record) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.cur == nil {
		return nil // no sink configured → drop (disabled/unconfigured)
	}
	return r.cur.Write(rec)
}

// swap installs s (may be nil to tear down) and closes the previous sink
// after no write can be holding it.
func (r *reloadableSink) swap(s Sink, log *slog.Logger) {
	r.mu.Lock()
	old := r.cur
	r.cur = s
	r.mu.Unlock()
	closeSink(old, log)
}

// Close tears down the current sink (emitter calls this after draining).
func (r *reloadableSink) Close() error {
	r.swap(nil, slog.Default())
	return nil
}

func closeSink(s Sink, log *slog.Logger) {
	if s == nil {
		return
	}
	if c, ok := s.(Closer); ok {
		if err := c.Close(); err != nil {
			log.Warn("payloadlog: sink close failed", "err", err)
		}
	}
}
