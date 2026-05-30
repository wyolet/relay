package usagelog

import (
	"context"
	"log/slog"
	"sync"

	"github.com/wyolet/relay/app/settings"
)

// Backend bundles the sink (write) and reader (read) a log backend provides.
// For clickhouse/postgres/valkey, Sink and Reader are the same instance; for
// file they differ. The composition root names the backend packages and
// builds this; everything else consumes it through the Controller.
type Backend struct {
	Sink   Sink
	Reader Reader
}

// BackendBuilder constructs the Backend for a resolved config. Injected by
// the composition root so backend packages stay named only there.
type BackendBuilder func(ctx context.Context, cfg settings.UsageLogging) (Backend, error)

// SettingsSource reads a live settings section + subscribes to changes.
// Satisfied by *app/catalog.Catalog.
type SettingsSource interface {
	Setting(section string) (any, bool)
	OnSettingsChange(section string, fn func())
}

// Controller owns the log backend's runtime lifecycle: a long-lived Emitter
// draining into a reloadable sink, plus a reloadable reader the control
// plane queries. It reconciles the backend (file ↔ clickhouse ↔ postgres ↔
// valkey) on settings changes without a restart — a reroute is a clean break
// (new events go to the new store; old data stays put, no migration).
//
// Unlike payload-logging there is no enable/maxbytes gate: logging is
// constant. The swap closes the old sink after in-flight writes drain
// (reloadableSink's RWMutex); the old reader (== old sink for ch/pg/valkey,
// or a resource-less file reader) needs no separate close.
type Controller struct {
	src     SettingsSource
	build   BackendBuilder
	emitter *Emitter
	rsink   *reloadableSink
	reader  *reloadableReader
	log     *slog.Logger
	kick    chan struct{}

	mu      sync.Mutex
	applied settings.UsageLogging
	has     bool
}

// NewController wires the emitter → reloadable-sink chain and the reloadable
// reader. The sink/reader start empty until the first reconcile.
func NewController(src SettingsSource, build BackendBuilder, log *slog.Logger) *Controller {
	if log == nil {
		log = slog.Default()
	}
	rsink := &reloadableSink{}
	return &Controller{
		src:     src,
		build:   build,
		emitter: NewEmitter(EmitterOptions{Logger: log}, rsink),
		rsink:   rsink,
		reader:  &reloadableReader{},
		log:     log,
		kick:    make(chan struct{}, 1),
	}
}

// Emitter returns the long-lived emitter (the collector registers on it).
func (c *Controller) Emitter() *Emitter { return c.emitter }

// Reader returns the stable reader handle the control plane queries; it
// delegates to whichever backend reader is currently live.
func (c *Controller) Reader() Reader { return c.reader }

// Subscribe registers the settings-change callback (cheap, non-blocking —
// signals the kick channel; the rebuild runs on Run's goroutine).
func (c *Controller) Subscribe() {
	c.src.OnSettingsChange(settings.SectionUsageLogging, c.signal)
}

func (c *Controller) signal() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// Run does the initial reconcile then reconciles on each kick until ctx ends.
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

// Close drains the emitter (which closes the live sink).
func (c *Controller) Close() { c.emitter.Close() }

func (c *Controller) reconcile(ctx context.Context) {
	cfg := c.current()
	c.mu.Lock()
	applied, has := c.applied, c.has
	c.mu.Unlock()
	if has && cfg == applied {
		return
	}

	be, err := c.build(ctx, cfg)
	if err != nil {
		c.log.Error("usagelog: backend build failed; keeping previous", "err", err, "backend", cfg.Backend)
		return // leave applied unchanged → retry on next kick
	}
	c.reader.swap(be.Reader)     // repoint reads (no close — see type doc)
	c.rsink.swap(be.Sink, c.log) // install + close old sink after writes drain

	c.mu.Lock()
	c.applied, c.has = cfg, true
	c.mu.Unlock()
	c.log.Info("usagelog: reconciled", "backend", cfg.Backend)
}

func (c *Controller) current() settings.UsageLogging {
	v, ok := c.src.Setting(settings.SectionUsageLogging)
	if !ok {
		return settings.UsageLogging{}
	}
	u, ok := v.(*settings.UsageLogging)
	if !ok || u == nil {
		return settings.UsageLogging{}
	}
	return *u
}

// reloadableSink is the Emitter's sink: it delegates to a swappable inner
// sink under an RWMutex so swap waits for in-flight writes before closing
// the old sink.
type reloadableSink struct {
	mu  sync.RWMutex
	cur Sink
}

func (r *reloadableSink) Write(ev Event) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.cur == nil {
		return nil
	}
	return r.cur.Write(ev)
}

func (r *reloadableSink) swap(s Sink, log *slog.Logger) {
	r.mu.Lock()
	old := r.cur
	r.cur = s
	r.mu.Unlock()
	closeSink(old, log)
}

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
			log.Warn("usagelog: sink close failed", "err", err)
		}
	}
}

// reloadableReader delegates the read methods to a swappable inner reader
// under an RWMutex. No close on swap: the inner reader is either the same
// instance as the sink (closed via reloadableSink) or a resource-less file
// reader.
type reloadableReader struct {
	mu  sync.RWMutex
	cur Reader
}

func (r *reloadableReader) swap(rd Reader) {
	r.mu.Lock()
	r.cur = rd
	r.mu.Unlock()
}

func (r *reloadableReader) Events(ctx context.Context, q EventQuery) ([]Event, error) {
	r.mu.RLock()
	cur := r.cur
	r.mu.RUnlock()
	if cur == nil {
		return nil, nil
	}
	return cur.Events(ctx, q)
}

func (r *reloadableReader) Summary(ctx context.Context, q SummaryQuery) (SummaryResult, error) {
	r.mu.RLock()
	cur := r.cur
	r.mu.RUnlock()
	if cur == nil {
		return SummaryResult{}, nil
	}
	return cur.Summary(ctx, q)
}

func (r *reloadableReader) TimeSeries(ctx context.Context, q TimeSeriesQuery) (TimeSeriesResult, error) {
	r.mu.RLock()
	cur := r.cur
	r.mu.RUnlock()
	if cur == nil {
		return TimeSeriesResult{}, nil
	}
	return cur.TimeSeries(ctx, q)
}
