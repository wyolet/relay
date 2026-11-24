package control

import (
	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/pkg/meta"
)

// registerUpstreamCRUD wires accounts, pools and budgets: the rows a policy
// draws keys from and counts spend against. Pools and budgets take their
// defaults before validation, so a stored row matches the manifest form.
func registerUpstreamCRUD(api huma.API, d Deps, protect huma.Middlewares, rows kindIndex) {
	ameta := func(a *account.Account) *meta.Metadata { return &a.Meta }
	plmeta := func(p *pool.Pool) *meta.Metadata { return &p.Meta }
	bumeta := func(b *budget.Budget) *meta.Metadata { return &b.Meta }
	c := refs(d)

	rows.add(registerKind[account.Account](
		api, "accounts", "account", accountRows{Store: d.Stores.Account, d: d}, sharerAuthz{Authorizer: d.Authz, d: d}, ameta,
		func(a *account.Account) error { return a.Validate() },
		meta.OwnerUser,
		listScanResolver(d.Stores.Account, ameta),
		guardAccount(d),
		d.enrichAccount,
		d.enrichAccounts,
		d.accountInUse,
		mergeAccountServerFields,
		d.Catalog,
		false,
		protect,
		&accountFilter,
		&usableFilter[account.Account]{checker: c, purposes: []usablePurpose[account.Account]{
			{name: "binding", linker: "a policy binding", kind: "policy-binding", check: refcheck.Checker.UsableAccount},
			{name: "pool", linker: "a pool", kind: "pool", check: refcheck.Checker.UsablePoolAccount},
		}},
	))

	plc := poolChecks(d)
	rows.add(registerKind[pool.Pool](
		api, plc.plural, plc.singular, d.Stores.Pool, d.Authz, plmeta,
		plc.validate,
		meta.OwnerUser,
		listScanResolver(d.Stores.Pool, plmeta),
		plc.guard,
		nil,
		nil,
		d.refuseInUse,
		nil,
		d.Catalog,
		false,
		protect,
		&poolFilter,
		usableFor(c, "policy", refcheck.Checker.UsablePool),
	))

	buc := budgetChecks(d)
	rows.add(registerKind[budget.Budget](
		api, buc.plural, buc.singular, d.Stores.Budget, d.Authz, bumeta,
		buc.validate,
		meta.OwnerUser,
		listScanResolver(d.Stores.Budget, bumeta),
		buc.guard,
		nil,
		nil,
		d.refuseInUse,
		nil,
		d.Catalog,
		false,
		protect,
		&budgetFilter,
		usableFor(c, "policy", refcheck.Checker.UsableBudget),
	))
}
