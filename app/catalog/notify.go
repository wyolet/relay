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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/overlay"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/metrics"
)

// ── payload types ─────────────────────────────────────────────────────────────

type notifyEvent struct {
	Kind string // "team", "project", "serviceaccount", "group", "provider", "host", "model", "hostkey", "ratelimit", "policy", "pricing", "relaykey"
	Op   string // "upsert" or "delete"
	ID   string
}

var validKinds = map[string]struct{}{
	"provider": {}, "host": {}, "model": {}, "hostkey": {},
	"ratelimit": {}, "policy": {}, "pricing": {}, "relaykey": {},
	"hostbinding": {}, "settings": {}, "overlay": {},
	"team": {}, "project": {},
	"serviceaccount": {}, "group": {},
	"role": {}, "rolebinding": {}, "policybinding": {},
	"user": {},
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
	key interface {
		Get(ctx context.Context, id string) (*key.Key, error)
	}
	overlay interface {
		Get(ctx context.Context, kind, resourceID string) (*overlay.Overlay, error)
	}
	team interface {
		Get(ctx context.Context, id string) (*team.Team, error)
	}
	serviceAccount interface {
		Get(ctx context.Context, id string) (*serviceaccount.ServiceAccount, error)
	}
	group interface {
		Get(ctx context.Context, id string) (*group.Group, error)
	}
	role interface {
		Get(ctx context.Context, id string) (*role.Role, error)
	}
	roleBinding interface {
		Get(ctx context.Context, id string) (*rolebinding.RoleBinding, error)
	}
	policyBinding interface {
		Get(ctx context.Context, id string) (*policybinding.PolicyBinding, error)
	}
	project interface {
		Get(ctx context.Context, id string) (*project.Project, error)
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

	// reloadPending marks a failed fallback reload; every flush retries it
	// until it succeeds. Touched only by the flush goroutine.
	reloadPending bool
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
// Order: team → project → provider → host → ratelimit → model → hostkey →
// policy → pricing → key. Deletes propagate via reverse-ref cascade
// inside the reconciler so they don't need a separate ordering pass.
func (l *Listener) applyDrained(ctx context.Context) {
	events := l.deb.drain()
	// Every incremental apply clones the whole snapshot, so a bulk write
	// (an apply of a large bundle) would clone once per row. Past this size
	// one full rebuild is cheaper and reaches the same state.
	if l.reloadPending || len(events) > reloadBatchThreshold {
		l.reloadAndApplySettings(ctx, events)
		return
	}
	sort.SliceStable(events, func(i, j int) bool {
		return kindOrder[events[i].Kind] < kindOrder[events[j].Kind]
	})
	var failed []drainedEvent
	for _, e := range events {
		if err := l.applyEvent(ctx, e); err != nil {
			slog.Error("catalog notify: apply error, falling back to reload", "kind", e.Kind, "id", e.ID, "op", e.Op, "err", err)
			metrics.CatalogApplyFailed(e.Kind)
			failed = append(failed, e)
		}
	}
	// The drained event is gone from the debouncer, so a failed apply would
	// leave the snapshot diverged from PG until the next write to that row.
	if len(failed) > 0 {
		l.reloadAndApplySettings(ctx, failed)
	}
}

// reloadAndApplySettings rebuilds the snapshot from PG and applies the
// settings events among events. Reload rebuilds catalog rows only — the
// settings cache is loaded separately, so its events must be applied here or
// they never land. Anything that still fails is retried on the next flush.
func (l *Listener) reloadAndApplySettings(ctx context.Context, events []drainedEvent) {
	if err := l.cat.Reload(ctx); err != nil {
		slog.Error("catalog notify: reload failed, retrying next flush", "events", len(events), "err", err)
		l.reloadPending = true
		for _, e := range events {
			if e.Kind == "settings" {
				l.deb.requeue(e)
			}
		}
		return
	}
	l.reloadPending = false
	for _, e := range events {
		if e.Kind != "settings" {
			continue
		}
		if err := l.applyEvent(ctx, e); err != nil {
			slog.Error("catalog notify: apply error, retrying next flush", "kind", e.Kind, "id", e.ID, "op", e.Op, "err", err)
			metrics.CatalogApplyFailed(e.Kind)
			l.deb.requeue(e)
		}
	}
	slog.Info("catalog notify: catalog reloaded", "events", len(events))
}

// reloadBatchThreshold is the drained-event count past which a full rebuild
// replaces the per-event incremental applies.
const reloadBatchThreshold = 64

var kindOrder = map[string]int{
	"team":      0,
	"project":   1,
	"provider":  2,
	"host":      3,
	"ratelimit": 4,
	"model":     5,
	"hostkey":   6,
	"policy":    7,
	// service accounts sanitize against policies, keys against accounts.
	"serviceaccount": 8,
	"group":          9,
	// roles before the bindings that grant them; bindings after the
	// policies and tenancy rows they are scoped to.
	"role":          10,
	"rolebinding":   11,
	"policybinding": 12,
	"pricing":       13,
	"hostbinding":   14,
	"relaykey":      15,
	"overlay":       16, // after model upserts so re-merges see fresh templates
	"settings":      17,
	"user":          18,
}
