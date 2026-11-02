// Package accountsync writes the caller accounts the plans call for: it reads the pending account places off each published plan view and creates, closes or reopens their rows through the account store. The rows come back to the plans through the usual change feed. Every pod runs one; the writes are idempotent, so pods racing on one place converge on one row.
//
// Out of scope: deciding what should exist (app/plan), deleting accounts (the foreign keys drop them with their team, project, user or service account), and provider accounts.
package accountsync

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/pkg/engine"
)

// Store is the account-store surface the writer needs; *account.Store satisfies it.
type Store interface {
	EnsureManaged(ctx context.Context, want *account.Account) (string, error)
	SetMembership(ctx context.Context, a *account.Account, closed, followsRoleBindings bool) error
}

// Syncer applies pending account places. OnPublish only hands the latest list over, so the engine's hooks never wait on Postgres.
type Syncer struct {
	Store   Store
	latest  atomic.Pointer[[]*plan.ManagedAccount]
	wake    chan struct{}
	started atomic.Bool
}

// New returns a Syncer writing through s.
func New(s Store) *Syncer {
	return &Syncer{Store: s, wake: make(chan struct{}, 1)}
}

// OnPublish takes the view's pending places; the newest list replaces one not yet worked.
func (s *Syncer) OnPublish(v *engine.View, _ engine.PublishStats) {
	items := plan.Pending(v)
	s.latest.Store(&items)
	if len(items) == 0 {
		return
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run works pending lists until ctx ends.
func (s *Syncer) Run(ctx context.Context) {
	if !s.started.CompareAndSwap(false, true) {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		}
		if p := s.latest.Load(); p != nil {
			s.apply(ctx, *p)
		}
	}
}

func (s *Syncer) apply(ctx context.Context, items []*plan.ManagedAccount) {
	for _, m := range items {
		if ctx.Err() != nil {
			return
		}
		var err error
		switch m.Action() {
		case plan.AccountCreate:
			_, err = s.Store.EnsureManaged(ctx, m.Want)
		// Both are conditional on the row's version the plan read: a list gone stale (the row reopened or closed since) writes nothing.
		case plan.AccountClose:
			err = s.Store.SetMembership(ctx, m.Row, true, true)
		case plan.AccountReopen:
			err = s.Store.SetMembership(ctx, m.Row, false, true)
		}
		if err != nil {
			// The next publish lists the place again.
			slog.Error("accounts: could not apply an account place", "place", m.Key, "err", err)
		}
	}
}
