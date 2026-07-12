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
// the composition root (the hostkey store in practice). It MUST exclude
// credentials whose stored status is revoked — renewal resumes when the
// operator re-authorizes (a value update clears the status), and the
// refresher itself keeps no memory of them.
type RefSource func(ctx context.Context) ([]secret.Ref, error)

// Hooks receive renewal outcomes so the composition root can persist
// observed credential state (hostkey status) and broadcast rotations.
// Either hook may be nil.
type Hooks struct {
	// OnRenewed fires after a rotated blob was persisted; expiresAt is the
	// fresh token's expiry.
	OnRenewed func(ctx context.Context, id string, expiresAt time.Time) error
	// OnRevoked fires when the provider rejected the grant itself
	// (IsPermanent) — re-authorization is an operator action.
	OnRevoked func(ctx context.Context, id string, cause error) error
}

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

	hooks Hooks

	log      *slog.Logger
	interval time.Duration
	lead     time.Duration
}

const (
	defaultInterval = time.Minute
	defaultLead     = 10 * time.Minute
)

// NewRefresher builds a Refresher. source, resolver, and locks are
// required.
func NewRefresher(source RefSource, resolver *Resolver, locks Locker, hooks Hooks, log *slog.Logger) *Refresher {
	if log == nil {
		log = slog.Default()
	}
	return &Refresher{
		source:   source,
		resolver: resolver,
		locks:    locks,
		hooks:    hooks,
		log:      log,
		interval: defaultInterval,
		lead:     defaultLead,
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
	for _, ref := range refs {
		if ref.Kind != secret.KindOAuth || ref.ID == "" {
			continue
		}
		f.renewOne(ctx, ref)
	}
}

func (f *Refresher) renewOne(ctx context.Context, ref secret.Ref) {
	var (
		rotated   bool
		expiresAt time.Time
	)
	lockKey := "{oauthrenew:" + ref.ID + "}"
	err := f.locks.WithLock(ctx, []string{lockKey}, func(ctx context.Context) error {
		var err error
		rotated, expiresAt, err = f.resolver.Renew(ctx, ref, f.lead)
		return err
	})
	switch {
	case err == nil && rotated:
		f.log.Info("oauth refresher: credential renewed",
			"id", ref.ID, "provider", ref.Provider, "expires_at", expiresAt)
		if f.hooks.OnRenewed != nil {
			if herr := f.hooks.OnRenewed(ctx, ref.ID, expiresAt); herr != nil {
				f.log.Error("oauth refresher: renewal hook failed; peers heal on their next resolve",
					"id", ref.ID, "err", herr)
			}
		}
	case err != nil && IsPermanent(err):
		// The stored status keeps this credential out of the source until
		// the operator re-authorizes; no refresher-side memory.
		f.log.Error("oauth refresher: grant rejected — re-authorization required",
			"id", ref.ID, "provider", ref.Provider, "err", err)
		if f.hooks.OnRevoked != nil {
			if herr := f.hooks.OnRevoked(ctx, ref.ID, err); herr != nil {
				f.log.Error("oauth refresher: revocation hook failed", "id", ref.ID, "err", herr)
			}
		}
	case err != nil:
		// Transient: the next sweep retries; the token stays valid until
		// its real expiry, and the KeyAgent heal covers the worst case.
		f.log.Warn("oauth refresher: renewal failed; will retry",
			"id", ref.ID, "provider", ref.Provider, "err", err)
	}
}
