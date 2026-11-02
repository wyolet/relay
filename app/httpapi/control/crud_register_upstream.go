package control

import (
	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/pool"
)

// registerUpstreamCRUD wires accounts, pools and budgets: the rows a policy
// draws keys from and counts spend against. Pools and budgets take their
// defaults before validation, so a stored row matches the manifest form.
func registerUpstreamCRUD(api huma.API, d Deps, protect huma.Middlewares, rows kindIndex) {
	ameta := func(a *account.Account) *meta.Metadata { return &a.Meta }
	plmeta := func(p *pool.Pool) *meta.Metadata { return &p.Meta }
	bumeta := func(b *budget.Budget) *meta.Metadata { return &b.Meta }

	rows.add(registerKind[account.Account](
		api, "accounts", "account", accountRows{Store: d.Stores.Account, d: d}, sharerAuthz{Authorizer: d.Authz, d: d}, ameta,
		func(a *account.Account) error { return a.Validate() },
		meta.OwnerUser,
		listScanResolver(d.Stores.Account, ameta),
		guardAccount(d),
		d.enrichAccount,
		nil,
		d.accountInUse,
		mergeAccountServerFields,
		d.Catalog,
		false,
		protect,
		&accountFilter,
	))

	rows.add(registerKind[pool.Pool](
		api, "pools", "pool", d.Stores.Pool, d.Authz, plmeta,
		func(p *pool.Pool) error { p.Default(); return p.Validate() },
		meta.OwnerUser,
		listScanResolver(d.Stores.Pool, plmeta),
		guardPool(d),
		nil,
		nil,
		d.refuseInUse,
		nil,
		d.Catalog,
		false,
		protect,
		&poolFilter,
	))

	rows.add(registerKind[budget.Budget](
		api, "budgets", "budget", d.Stores.Budget, d.Authz, bumeta,
		func(b *budget.Budget) error { b.Default(); return b.Validate() },
		meta.OwnerUser,
		listScanResolver(d.Stores.Budget, bumeta),
		guardBudgetMove(d),
		nil,
		nil,
		d.refuseInUse,
		nil,
		d.Catalog,
		false,
		protect,
		&budgetFilter,
	))
}
