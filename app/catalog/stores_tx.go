package catalog

import (
	"context"
	"errors"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/catalogstore"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/overlay"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	authpg "github.com/wyolet/relay/auth/store/postgres"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
)

// rowStores wires every store that needs nothing but a database: the pool at
// boot, or a transaction in InTx.
func rowStores(db storage.DB) *Stores {
	q := gen.New(db)
	return &Stores{
		Provider:  catalogstore.NewProviderStore(q),
		Host:      catalogstore.NewHostStore(q),
		Model:     catalogstore.NewModelStore(q),
		RateLimit: catalogstore.NewRateLimitStore(q),
		Policy:    catalogstore.NewPolicyStore(db),
		Pricing:   catalogstore.NewPricingStore(db),
		Binding:   catalogstore.NewBindingStore(db),
		Key:       key.NewStore(q),
		Overlay:   overlay.NewStore(q),
		Settings:  settings.NewStore(q),
		Team:      team.NewStore(db),
		Project:   project.NewStore(db),
		Account:   account.NewStore(q),
		Pool:      pool.NewStore(db),
		Budget:    budget.NewStore(q),

		ServiceAccount: serviceaccount.NewStore(db),
		Group:          group.NewStore(db),
		Role:           role.NewStore(db),
		RoleBinding:    rolebinding.NewStore(db),
		PolicyBinding:  policybinding.NewStore(db),
		Users:          user.NewStore(db),
		Directory:      authpg.New(db),
		SCIMClients:    authpg.NewSCIMTokens(db),
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
