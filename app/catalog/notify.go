// notify.go implements the PG NOTIFY listener and debouncer for catalog events.
//
// The listener acquires a dedicated connection from the pool, issues
// LISTEN catalog_events, and forwards parsed events to the debouncer.
// A flush goroutine drains the debouncer every second, fetches each
// affected row via the narrow store interfaces, and applies it to the
// Catalog via the appropriate Apply* method.
//
// On any connection error the listener logs, waits 1 s, and reconnects —
// it never panics. Run blocks until ctx is cancelled.
package catalog

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
)

// ── payload types ─────────────────────────────────────────────────────────────

type notifyEvent struct {
	Kind string // "provider", "host", "model", "hostkey", "ratelimit", "policy", "pricing", "relaykey"
	Op   string // "upsert" or "delete"
	ID   string
}

var validKinds = map[string]struct{}{
	"provider": {}, "host": {}, "model": {}, "hostkey": {},
	"ratelimit": {}, "policy": {}, "pricing": {}, "relaykey": {},
	"settings": {},
}

// parseEvent splits "kind:op:id". The id is the remainder after the second
// colon and may itself contain colons — settings section keys are
// colon-namespaced (e.g. "governance:policy"), so the payload
// "settings:upsert:governance:policy" yields id "governance:policy". Returns
// false on any malformed input.
func parseEvent(payload string) (notifyEvent, bool) {
	parts := strings.SplitN(payload, ":", 3)
	if len(parts) != 3 {
		return notifyEvent{}, false
	}
	kind, op, id := parts[0], parts[1], parts[2]
	if _, ok := validKinds[kind]; !ok {
		return notifyEvent{}, false
	}
	if op != "upsert" && op != "delete" {
		return notifyEvent{}, false
	}
	if id == "" {
		return notifyEvent{}, false
	}
	return notifyEvent{Kind: kind, Op: op, ID: id}, true
}

// ── debouncer ─────────────────────────────────────────────────────────────────

type eventKey struct{ Kind, ID string }

const debounceCap = 1000

type debouncer struct {
	mu       sync.Mutex
	pending  map[eventKey]string // value = op; last-write-wins
	interval time.Duration
}

func newDebouncer(interval time.Duration) *debouncer {
	return &debouncer{
		pending:  make(map[eventKey]string, 64),
		interval: interval,
	}
}

// push records an event. Returns true if the buffer hit the soft cap and
// the caller should trigger an immediate flush.
func (d *debouncer) push(e notifyEvent) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pending[eventKey{e.Kind, e.ID}] = e.Op
	return len(d.pending) >= debounceCap
}

type drainedEvent struct {
	Kind string
	ID   string
	Op   string
}

// drain atomically extracts all pending events and returns them as a slice.
func (d *debouncer) drain() []drainedEvent {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.pending) == 0 {
		return nil
	}
	out := make([]drainedEvent, 0, len(d.pending))
	for k, op := range d.pending {
		out = append(out, drainedEvent{Kind: k.Kind, ID: k.ID, Op: op})
	}
	d.pending = make(map[eventKey]string, 64)
	return out
}

// ── narrow store interfaces ───────────────────────────────────────────────────

type listenerStores struct {
	provider interface {
		Get(ctx context.Context, id string) (*provider.Provider, error)
	}
	host interface {
		Get(ctx context.Context, id string) (*host.Host, error)
	}
	model interface {
		Get(ctx context.Context, id string) (*model.Model, error)
	}
	hostkey interface {
		Get(ctx context.Context, id string) (*hostkey.HostKey, error)
	}
	ratelimit interface {
		Get(ctx context.Context, id string) (*ratelimit.RateLimit, error)
	}
	policy interface {
		Get(ctx context.Context, id string) (*policy.Policy, error)
	}
	pricing interface {
		Get(ctx context.Context, id string) (*pricing.Pricing, error)
	}
	relaykey interface {
		Get(ctx context.Context, id string) (*relaykey.RelayKey, error)
	}
	settings SettingsLister
}

// ── Listener ──────────────────────────────────────────────────────────────────

// Listener subscribes to catalog_events NOTIFY, debounces the payload stream,
// and applies incremental updates to the Catalog.
type Listener struct {
	cat    *Catalog
	pool   *pgxpool.Pool
	stores listenerStores
	deb    *debouncer
}

// NewListener constructs a Listener. Call Run to start it.
func NewListener(cat *Catalog, pool *pgxpool.Pool, stores listenerStores) *Listener {
	return &Listener{
		cat:    cat,
		pool:   pool,
		stores: stores,
		deb:    newDebouncer(time.Second),
	}
}

// Run blocks until ctx is cancelled. It reconnects on any connection error.
func (l *Listener) Run(ctx context.Context) error {
	// Start the flush goroutine.
	flushCh := make(chan struct{}, 1)
	go l.flushLoop(ctx, flushCh)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := l.listen(ctx, flushCh); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Error("catalog notify: connection error, reconnecting", "err", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
}

// listen acquires a connection, issues LISTEN, and forwards events to the
// debouncer until an error occurs or ctx is cancelled.
func (l *Listener) listen(ctx context.Context, flushCh chan<- struct{}) error {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN catalog_events"); err != nil {
		return err
	}
	slog.Info("catalog notify: listening on catalog_events")

	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		ev, ok := parseEvent(n.Payload)
		if !ok {
			slog.Warn("catalog notify: malformed payload", "payload", n.Payload)
			continue
		}
		if capped := l.deb.push(ev); capped {
			// Non-blocking nudge to flush early.
			select {
			case flushCh <- struct{}{}:
			default:
			}
		}
	}
}

// flushLoop drains and applies the debouncer on a 1-second ticker or when
// nudged via flushCh.
func (l *Listener) flushLoop(ctx context.Context, flushCh <-chan struct{}) {
	ticker := time.NewTicker(l.deb.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.applyDrained(ctx)
		case <-flushCh:
			l.applyDrained(ctx)
		}
	}
}

// applyDrained drains the debouncer and applies each event in dependency
// order: parent kinds before their children, so that when a bulk admin
// transaction commits and the debouncer flushes the whole burst together,
// cross-ref validation against the snapshot succeeds at each step.
//
// Order: provider → host → ratelimit → model → hostkey → policy → pricing
// → relaykey. Deletes propagate via reverse-ref cascade inside the
// reconciler so they don't need a separate ordering pass.
func (l *Listener) applyDrained(ctx context.Context) {
	events := l.deb.drain()
	sort.SliceStable(events, func(i, j int) bool {
		return kindOrder[events[i].Kind] < kindOrder[events[j].Kind]
	})
	for _, e := range events {
		if err := l.applyEvent(ctx, e); err != nil {
			slog.Error("catalog notify: apply error", "kind", e.Kind, "id", e.ID, "op", e.Op, "err", err)
		}
	}
}

var kindOrder = map[string]int{
	"provider":  0,
	"host":      1,
	"ratelimit": 2,
	"model":     3,
	"hostkey":   4,
	"policy":    5,
	"pricing":   6,
	"relaykey":  7,
	"settings":  8,
}

// applyEvent fetches the row (for upserts) and calls the appropriate Apply* method.
func (l *Listener) applyEvent(ctx context.Context, e drainedEvent) error {
	switch e.Kind {
	case "provider":
		if e.Op == "delete" {
			return l.cat.ApplyProviderDelete(e.ID)
		}
		p, err := l.stores.provider.Get(ctx, e.ID)
		if err != nil {
			return err
		}
		if p == nil {
			return l.cat.ApplyProviderDelete(e.ID)
		}
		return l.cat.ApplyProviderUpsert(p)

	case "host":
		if e.Op == "delete" {
			return l.cat.ApplyHostDelete(e.ID)
		}
		h, err := l.stores.host.Get(ctx, e.ID)
		if err != nil {
			return err
		}
		if h == nil {
			return l.cat.ApplyHostDelete(e.ID)
		}
		return l.cat.ApplyHostUpsert(h)

	case "model":
		if e.Op == "delete" {
			return l.cat.ApplyModelDelete(e.ID)
		}
		m, err := l.stores.model.Get(ctx, e.ID)
		if err != nil {
			return err
		}
		if m == nil {
			return l.cat.ApplyModelDelete(e.ID)
		}
		return l.cat.ApplyModelUpsert(m)

	case "hostkey":
		if e.Op == "delete" {
			return l.cat.ApplyHostKeyDelete(e.ID)
		}
		k, err := l.stores.hostkey.Get(ctx, e.ID)
		if err != nil {
			return err
		}
		if k == nil {
			return l.cat.ApplyHostKeyDelete(e.ID)
		}
		return l.cat.ApplyHostKeyUpsert(k)

	case "ratelimit":
		if e.Op == "delete" {
			return l.cat.ApplyRateLimitDelete(e.ID)
		}
		r, err := l.stores.ratelimit.Get(ctx, e.ID)
		if err != nil {
			return err
		}
		if r == nil {
			return l.cat.ApplyRateLimitDelete(e.ID)
		}
		return l.cat.ApplyRateLimitUpsert(r)

	case "policy":
		if e.Op == "delete" {
			return l.cat.ApplyPolicyDelete(e.ID)
		}
		p, err := l.stores.policy.Get(ctx, e.ID)
		if err != nil {
			return err
		}
		if p == nil {
			return l.cat.ApplyPolicyDelete(e.ID)
		}
		return l.cat.ApplyPolicyUpsert(p)

	case "pricing":
		if e.Op == "delete" {
			return l.cat.ApplyPricingDelete(e.ID)
		}
		p, err := l.stores.pricing.Get(ctx, e.ID)
		if err != nil {
			return err
		}
		if p == nil {
			return l.cat.ApplyPricingDelete(e.ID)
		}
		return l.cat.ApplyPricingUpsert(p)

	case "relaykey":
		if e.Op == "delete" {
			return l.cat.ApplyRelayKeyDelete(e.ID)
		}
		k, err := l.stores.relaykey.Get(ctx, e.ID)
		if err != nil {
			return err
		}
		if k == nil {
			return l.cat.ApplyRelayKeyDelete(e.ID)
		}
		return l.cat.ApplyRelayKeyUpsert(k)

	case "settings":
		if e.Op == "delete" {
			l.cat.settings.applyDelete(e.ID)
			return nil
		}
		return l.cat.settings.applyUpsert(ctx, e.ID)
	}
	return nil
}
