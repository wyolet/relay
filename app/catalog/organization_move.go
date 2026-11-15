package catalog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

// moveCounts is what one pass of moveToOrganization did, per kind. Failed rows stay system-owned and are tried again on the next boot.
type moveCounts struct {
	Moved  map[string]int
	Kept   map[string]int
	Failed map[string]int
}

func (m *moveCounts) count(into map[string]int, kind string) { into[kind]++ }

// runOrganizationMove runs moveToOrganization on one pod at a time and logs what it did. A failed pass leaves rows where they were and never blocks boot: the next boot tries again.
func runOrganizationMove(ctx context.Context, stores *Stores, opts BootstrapOptions) {
	var res *moveCounts
	run := func(ctx context.Context) error {
		var err error
		res, err = moveToOrganization(ctx, stores, opts)
		return err
	}
	var err error
	if opts.SeedLock != nil {
		err = opts.SeedLock(ctx, run)
	} else {
		err = run(ctx)
	}
	if err != nil {
		slog.Error("organization move: pass stopped; the rows it had not reached stay system-owned", "err", err)
	}
	if res != nil && (len(res.Moved) > 0 || len(res.Failed) > 0) {
		slog.Info("organization move: moved system-owned rows to the organization", "moved", res.Moved, "kept_built_in", res.Kept, "failed", res.Failed)
	}
}

// moveToOrganization moves the deployment's rows still owned by system to the organization, after the schema migration moved every row it could decide on alone: every system policy, budget and pool, every system rate limit but the built-ins this binary ships, and host keys, whose stored value is sealed again under the organization's data key. The catalog ships none of these as system rows (its policies and rate limits are host-owned; catalogvalidate refuses a system one), so no tree is needed to tell them apart, and a moved row loses its detach mark. Each row moves on its own, through its store's Upsert at the version just read, so a row edited meanwhile or a value that does not open is left as it was. Running it again moves nothing more.
func moveToOrganization(ctx context.Context, stores *Stores, opts BootstrapOptions) (*moveCounts, error) {
	res := &moveCounts{Moved: map[string]int{}, Kept: map[string]int{}, Failed: map[string]int{}}
	builtin := map[string]bool{}
	for _, rl := range opts.SystemRateLimits {
		builtin[rl.Meta.Name] = true
	}
	none := func(*meta.Metadata) bool { return false }
	err := errors.Join(
		moveRows(ctx, res, "Policy", stores.Policy.List, func(p *policy.Policy) *meta.Metadata { return &p.Meta }, stores.Policy.Upsert, none),
		moveRows(ctx, res, "RateLimit", stores.RateLimit.List, func(r *ratelimit.RateLimit) *meta.Metadata { return &r.Meta }, stores.RateLimit.Upsert,
			func(m *meta.Metadata) bool {
				// Without the binary's definitions a built-in row cannot be told apart, and moving one would stop its limit.
				return len(builtin) == 0 || builtin[m.Name]
			}),
		moveRows(ctx, res, "Budget", stores.Budget.List, func(b *budget.Budget) *meta.Metadata { return &b.Meta }, stores.Budget.Upsert, none),
		moveRows(ctx, res, "Pool", stores.Pool.List, func(p *pool.Pool) *meta.Metadata { return &p.Meta }, stores.Pool.Upsert, none),
	)
	if err != nil {
		return res, err
	}
	return res, moveHostKeys(ctx, res, stores)
}

// moveRows moves every system-owned row of one kind that keep does not hold back.
func moveRows[T any](ctx context.Context, res *moveCounts, kind string,
	list func(context.Context) ([]*T, error), metaOf func(*T) *meta.Metadata,
	upsert func(context.Context, *T) error, keep func(*meta.Metadata) bool,
) error {
	rows, err := list(ctx)
	if err != nil {
		return fmt.Errorf("list %s: %w", kind, err)
	}
	for _, row := range rows {
		m := metaOf(row)
		if m.Owner.Kind != meta.OwnerSystem {
			continue
		}
		if keep(m) {
			res.count(res.Kept, kind)
			continue
		}
		m.Owner, m.CatalogDetachedAt = meta.Global, nil
		if err := upsert(ctx, row); err != nil {
			res.count(res.Failed, kind)
			slog.Error("organization move: row left system-owned", "kind", kind, "id", m.ID, "name", m.Name, "err", err)
			continue
		}
		res.count(res.Moved, kind)
	}
	return nil
}

// moveHostKeys moves every system-owned host key. A stored value is sealed under the organization's data key in the transaction that changes the owner and decrypted again before it commits; one that does not open leaves the key as it was.
func moveHostKeys(ctx context.Context, res *moveCounts, stores *Stores) error {
	keys, err := stores.HostKey.List(ctx)
	if err != nil {
		return fmt.Errorf("list HostKey: %w", err)
	}
	for _, k := range keys {
		if k.Meta.Owner.Kind != meta.OwnerSystem {
			continue
		}
		if k.Spec.ValueFrom.Kind != hostkey.ValueKindEnv && stores.Stored == nil {
			res.count(res.Failed, "HostKey")
			slog.Error("organization move: host key left system-owned; moving a stored value needs RELAY_MASTER_KEY", "id", k.Meta.ID, "name", k.Meta.Name)
			continue
		}
		if err := stores.HostKey.ChangeOwner(ctx, k, meta.Global); err != nil {
			res.count(res.Failed, "HostKey")
			slog.Error("organization move: host key left system-owned", "id", k.Meta.ID, "name", k.Meta.Name, "err", err)
			continue
		}
		res.count(res.Moved, "HostKey")
	}
	return nil
}
