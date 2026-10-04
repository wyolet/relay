package catalog

import (
	"context"
	"errors"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
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
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
)

// rowStores wires every store that needs nothing but a database: the pool at
// boot, or a transaction in InTx.
func rowStores(db storage.DB) *Stores {
	q := gen.New(db)
	return &Stores{
		Provider:  provider.NewStore(q),
		Host:      host.NewStore(q),
		Model:     model.NewStore(q),
		RateLimit: ratelimit.NewStore(q),
		Policy:    policy.NewStore(db),
		Pricing:   pricing.NewStore(db),
		Binding:   binding.NewStore(db),
		Key:       key.NewStore(q),
		Overlay:   overlay.NewStore(q),
		Settings:  settings.NewStore(q),
		Team:      team.NewStore(q),
		Project:   project.NewStore(q),

		ServiceAccount: serviceaccount.NewStore(q),
		Group:          group.NewStore(db),
		Role:           role.NewStore(q),
		RoleBinding:    rolebinding.NewStore(db),
		PolicyBinding:  policybinding.NewStore(db),
		Users:          user.NewStore(q),
	}
}

// InTx runs fn with the stores bound to one transaction (see storage.InTx):
// every write fn makes commits together, or none does. HostKey is the
// pool-bound store, for reads only — a stored key's secret is written through
// the secret backends' own connections and cannot join the transaction.
func (s *Stores) InTx(ctx context.Context, fn func(ctx context.Context, tx *Stores) error) error {
	if s.pool == nil {
		return errors.New("catalog: stores are not bound to a database")
	}
	return storage.InTx(ctx, s.pool, func(ctx context.Context, db storage.DB) error {
		tx := rowStores(db)
		tx.HostKey = s.HostKey
		return fn(ctx, tx)
	})
}
