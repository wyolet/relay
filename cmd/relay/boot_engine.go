package main

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/budget"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/config"
	storagemod "github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/engine/cell"
)

// planDebounce matches the catalog listener's flush interval, so one admin write lands in the snapshot and the plans within the same second.
const planDebounce = time.Second

// planEngine is the engine that keeps the request path's plans current, with the cell registry its plans link and the holder the request path loads.
type planEngine struct {
	engine *engine.Engine
	cells  *cell.Registry
	plans  *plan.Holder
	idle   time.Duration
}

// buildPlanEngine registers the plan kinds and controllers over the PG stores. Nothing is loaded until load.
func buildPlanEngine(cfg *config.Config, st *storagemod.Storage, stores *appcatalog.Stores, users *user.Store) *planEngine {
	pe := &planEngine{plans: &plan.Holder{}, idle: cfg.PlanCellIdle}
	pe.cells = cell.NewRegistry(cell.Options{IdleTTL: cfg.PlanCellIdle})
	pe.engine = engine.New(engine.Options{
		Debounce: planDebounce,
		Cells:    pe.cells,
		Hooks: engine.Hooks{
			OnPublish: pe.plans.OnPublish,
			OnReconcileError: func(controller string, out engine.Ref, err error) {
				slog.Error("plans: reconcile failed; the previous plan keeps serving", "controller", controller, "kind", out.Kind, "id", out.ID, "err", err)
			},
			OnBatchAbandoned: func(gen engine.Generation, rounds int) {
				slog.Error("plans: change batch did not settle; resyncing", "generation", gen, "rounds", rounds)
			},
		},
	})
	q := gen.New(st.Pool())
	src := plan.Sources{
		Providers:       stores.Provider,
		Hosts:           stores.Host,
		Models:          stores.Model,
		Bindings:        stores.Binding,
		Pricings:        stores.Pricing,
		Policies:        stores.Policy,
		PolicyBindings:  stores.PolicyBinding,
		RateLimits:      stores.RateLimit,
		Budgets:         budget.NewStore(q),
		Pools:           pool.NewStore(st.Pool()),
		Accounts:        account.NewStore(q),
		Credentials:     stores.HostKey,
		Teams:           stores.Team,
		Projects:        stores.Project,
		ServiceAccounts: stores.ServiceAccount,
		Users:           users,
		Keys:            stores.Key,
	}
	if stores.Overlay != nil {
		src.Overlays = plan.OverlaySource{Store: stores.Overlay}
	}
	plan.Register(pe.engine, pe.cells, src)
	return pe
}

// load lists every kind and publishes the first plans; the request path refuses until it has.
func (pe *planEngine) load(ctx context.Context) error {
	start := time.Now()
	if err := pe.engine.Load(ctx); err != nil {
		return err
	}
	slog.Info("plans: loaded", "generation", pe.engine.View().Gen(), "duration", time.Since(start))
	return nil
}

// expireCells drops runtime state no plan has linked for the idle window, until ctx ends.
func (pe *planEngine) expireCells(ctx context.Context) {
	t := time.NewTicker(max(pe.idle/4, time.Second))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			pe.cells.Expire(now)
		}
	}
}

// Event forwards one catalog notification for a kind the plans read; other kinds are ignored.
func (pe *planEngine) Event(kind, op, id string) {
	ref, ok := planRef(kind, id)
	if !ok {
		return
	}
	ev := engine.Event{Ref: ref, Op: engine.OpUpsert}
	if op == "delete" {
		ev.Op = engine.OpDelete
	}
	pe.engine.Notify(ev)
}

// Attached resyncs: notifications sent while the listener was detached never arrive.
func (pe *planEngine) Attached(ctx context.Context) {
	if err := pe.engine.Resync(ctx); err != nil {
		slog.Error("plans: resync after the listener attached failed", "err", err)
	}
}

// planRef maps a catalog notification to the engine row it names.
func planRef(kind, id string) (engine.Ref, bool) {
	switch kind {
	case "provider", "host", "model", "pricing", "policy", "policybinding", "ratelimit",
		"budget", "pool", "account", "team", "project", "serviceaccount", "user":
		return engine.Ref{Kind: engine.Kind(kind), ID: id}, true
	case "hostkey":
		return engine.Ref{Kind: plan.KindCredential, ID: id}, true
	case "relaykey":
		return engine.Ref{Kind: plan.KindKey, ID: id}, true
	case "hostbinding":
		return engine.Ref{Kind: plan.KindBinding, ID: id}, true
	case "overlay":
		// The overlay payload id is "<kind>|<resource id>".
		k, rid, ok := strings.Cut(id, "|")
		if !ok {
			return engine.Ref{}, false
		}
		return engine.Ref{Kind: plan.KindOverlay, ID: plan.OverlayID(k, rid)}, true
	}
	return engine.Ref{}, false
}
