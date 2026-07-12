// refresher.go: proactive, cluster-safe renewal of OAuth credentials.
//
// The Refresher keeps subscription tokens fresh ahead of expiry so no
// request ever pays refresh latency (or a wasted upstream 401) — the
// KeyAgent reactive heal stays underneath as the safety net. It is fully
// isolated: everything it touches is injected (which refs exist, a lock,
// a rotation broadcast), it runs its own ticker goroutine, and the request
// pipeline knows nothing about it.
//
// Cluster safety: refresh tokens rotate on use, and a concurrent second
// refresh with the superseded token can trip the provider's reuse
// detection and revoke the whole grant. Every renewal therefore runs under
// a per-credential advisory lock (kv WithLock in production), with Renew
// re-loading the blob inside the critical section — the loser of a lock
// race sees the fresh blob and no-ops.
package oauth

import (
	"context"
	"log/slog"
	"time"

	"github.com/wyolet/relay/pkg/secret"
)

// RefSource enumerates the OAuth credentials to keep fresh. Injected by
// the composition root (the hostkey store in practice).
type RefSource func(ctx context.Context) ([]secret.Ref, error)

// Locker serializes a critical section across processes. pkg/kv.Store
// satisfies it; every key this package passes shares one credential-scoped
// hash tag per the kv conventions.
type Locker interface {
	WithLock(ctx context.Context, keys []string, fn func(context.Context) error) error
}

// Refresher periodically renews every sourced OAuth credential whose
// expiry falls within Lead. Construct with NewRefresher and start with Run.
type Refresher struct {
	source   RefSource
	resolver *Resolver
	locks    Locker

	// notify broadcasts a rotated credential id so peers reload it (the
	// composition root wires a catalog hostkey NOTIFY emit). Optional.
	notify func(ctx context.Context, id string) error

	log      *slog.Logger
	interval time.Duration
	lead     time.Duration
	deadFor  time.Duration

	// dead parks credentials whose refresh failed permanently
	// (IsPermanent) until deadFor elapses or the process restarts —
	// re-authorization is an operator action; hammering the token endpoint
	// only risks provider-side lockouts. Per-process by design: the
	// cross-process lock already serializes the (rare) retries.
	dead map[string]time.Time
}

const (
	defaultInterval = time.Minute
	defaultLead     = 10 * time.Minute
	defaultDeadFor  = time.Hour
)

// NewRefresher builds a Refresher. source, resolver, and locks are
// required; notify may be nil (single-pod deployments heal via their own
// resolve path).
func NewRefresher(source RefSource, resolver *Resolver, locks Locker, notify func(ctx context.Context, id string) error, log *slog.Logger) *Refresher {
	if log == nil {
		log = slog.Default()
	}
	return &Refresher{
		source:   source,
		resolver: resolver,
		locks:    locks,
		notify:   notify,
		log:      log,
		interval: defaultInterval,
		lead:     defaultLead,
		deadFor:  defaultDeadFor,
		dead:     map[string]time.Time{},
	}
}

// Run sweeps on a fixed interval until ctx is cancelled. Blocking — start
// it on its own goroutine.
func (f *Refresher) Run(ctx context.Context) {
	t := time.NewTicker(f.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.sweep(ctx)
		}
	}
}

// sweep renews every due credential once. Errors are contained per
// credential — one dead subscription never stops the others' renewals.
func (f *Refresher) sweep(ctx context.Context) {
	refs, err := f.source(ctx)
	if err != nil {
		f.log.Error("oauth refresher: list credentials", "err", err)
		return
	}
	now := time.Now()
	for _, ref := range refs {
		if ref.Kind != secret.KindOAuth || ref.ID == "" {
			continue
		}
		if until, parked := f.dead[ref.ID]; parked {
			if now.Before(until) {
				continue
			}
			delete(f.dead, ref.ID)
		}
		f.renewOne(ctx, ref)
	}
}

func (f *Refresher) renewOne(ctx context.Context, ref secret.Ref) {
	var rotated bool
	lockKey := "{oauthrenew:" + ref.ID + "}"
	err := f.locks.WithLock(ctx, []string{lockKey}, func(ctx context.Context) error {
		var err error
		rotated, err = f.resolver.Renew(ctx, ref, f.lead)
		return err
	})
	switch {
	case err == nil && rotated:
		f.log.Info("oauth refresher: credential renewed", "id", ref.ID, "provider", ref.Provider)
		if f.notify != nil {
			if nerr := f.notify(ctx, ref.ID); nerr != nil {
				f.log.Error("oauth refresher: rotation broadcast failed; peers heal on their next resolve",
					"id", ref.ID, "err", nerr)
			}
		}
	case err != nil && IsPermanent(err):
		f.dead[ref.ID] = time.Now().Add(f.deadFor)
		f.log.Error("oauth refresher: grant rejected — re-authorization required",
			"id", ref.ID, "provider", ref.Provider, "retry_after", f.deadFor, "err", err)
	case err != nil:
		// Transient: the next sweep retries; the token stays valid until
		// its real expiry, and the KeyAgent heal covers the worst case.
		f.log.Warn("oauth refresher: renewal failed; will retry",
			"id", ref.ID, "provider", ref.Provider, "err", err)
	}
}
