package apply

import (
	"context"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	apppool "github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/refcheck"
)

// catalogDocs are the catalog and upstream documents of one bundle, by kind.
type catalogDocs struct {
	providers  []*manifest.ProviderDTO
	hosts      []*manifest.HostDTO
	accounts   []*manifest.AccountDTO
	rateLimits []*manifest.RateLimitDTO
	budgets    []*manifest.BudgetDTO
	hostKeys   []*manifest.HostKeyDTO
	pools      []*manifest.PoolDTO
	models     []*manifest.ModelDTO
	pricings   []*manifest.PricingDTO
	bindings   []*manifest.HostBindingDTO
	policies   []*manifest.PolicyDTO
}

// planCatalog plans the kinds in the order their references run: a host
// before its accounts, an account before its keys, keys before the pools
// holding them, and every rate limit, budget, pool and key before the
// policies linking them. Prunes flush in the reverse order.
func (b *builder) planCatalog(ctx context.Context, d catalogDocs) error {
	s := b.opts.Stores
	if err := planKind(ctx, b, kindWiring[manifest.ProviderDTO, provider.Provider]{
		Kind: "Provider", Docs: d.providers, Names: b.idx.Providers, Rows: b.rows.Providers,
		To: manifest.ToProvider, Meta: func(p *provider.Provider) *meta.Metadata { return &p.Meta },
		Upsert: s.Provider.Upsert, Delete: s.Provider.Delete,
	}); err != nil {
		return err
	}
	if err := planKind(ctx, b, kindWiring[manifest.HostDTO, host.Host]{
		Kind: "Host", Docs: d.hosts, Names: b.idx.Hosts, Rows: b.rows.Hosts,
		To: manifest.ToHost, Meta: func(h *host.Host) *meta.Metadata { return &h.Meta },
		Upsert: s.Host.Upsert, Delete: s.Host.Delete,
	}); err != nil {
		return err
	}
	if err := planKind(ctx, b, kindWiring[manifest.AccountDTO, account.Account]{
		Kind: "Account", Docs: d.accounts, Names: b.idx.Accounts, Rows: b.rows.Accounts,
		To: manifest.ToAccount, Meta: func(a *account.Account) *meta.Metadata { return &a.Meta },
		Upsert: s.Account.Upsert, Delete: s.Account.Delete,
		Check: refsFor(b, refcheck.Checker.Account),
	}); err != nil {
		return err
	}
	if err := planKind(ctx, b, kindWiring[manifest.RateLimitDTO, ratelimit.RateLimit]{
		Kind: "RateLimit", Docs: d.rateLimits, Names: b.idx.RateLimits, Rows: b.rows.RateLimits,
		To: manifest.ToRateLimit, Meta: func(r *ratelimit.RateLimit) *meta.Metadata { return &r.Meta },
		Upsert: s.RateLimit.Upsert, Delete: s.RateLimit.Delete,
	}); err != nil {
		return err
	}
	if err := planKind(ctx, b, kindWiring[manifest.BudgetDTO, budget.Budget]{
		Kind: "Budget", Docs: d.budgets, Names: b.idx.Budgets, Rows: b.rows.Budgets,
		To: manifest.ToBudget, Meta: func(x *budget.Budget) *meta.Metadata { return &x.Meta },
		Upsert: s.Budget.Upsert, Delete: s.Budget.Delete,
	}); err != nil {
		return err
	}
	// The host-key rule runs in every load, the boot seed included: a key
	// naming an account at another host is refused, as the API refuses it.
	if err := planKind(ctx, b, kindWiring[manifest.HostKeyDTO, hostkey.HostKey]{
		Kind: "HostKey", Docs: d.hostKeys, Names: b.idx.HostKeys, Rows: b.rows.HostKeys,
		To: manifest.ToHostKey, Meta: func(k *hostkey.HostKey) *meta.Metadata { return &k.Meta },
		Upsert: s.HostKey.Upsert, Delete: s.HostKey.Delete,
		Check: b.refs().HostKey, Keep: func(prev, next *hostkey.HostKey) error { keepHostKeySecret(prev, next); return nil },
	}); err != nil {
		return err
	}
	if err := planKind(ctx, b, kindWiring[manifest.PoolDTO, apppool.Pool]{
		Kind: "Pool", Docs: d.pools, Names: b.idx.Pools, Rows: b.rows.Pools,
		To: manifest.ToPool, Meta: func(p *apppool.Pool) *meta.Metadata { return &p.Meta },
		Upsert: s.Pool.Upsert, Delete: s.Pool.Delete,
		Check: b.refs().Pool,
	}); err != nil {
		return err
	}
	if err := planKind(ctx, b, kindWiring[manifest.ModelDTO, model.Model]{
		Kind: "Model", Docs: d.models, Names: b.idx.Models, Rows: b.rows.Models,
		To: manifest.ToModel, Meta: func(m *model.Model) *meta.Metadata { return &m.Meta },
		Upsert: s.Model.Upsert, Delete: s.Model.Delete,
	}); err != nil {
		return err
	}
	if err := planKind(ctx, b, kindWiring[manifest.PricingDTO, pricing.Pricing]{
		Kind: "Pricing", Docs: d.pricings, Names: b.idx.Pricings, Rows: b.rows.Pricings,
		To: manifest.ToPricing, Meta: func(p *pricing.Pricing) *meta.Metadata { return &p.Meta },
		Upsert: s.Pricing.Upsert, Delete: s.Pricing.Delete,
	}); err != nil {
		return err
	}
	if err := planKind(ctx, b, kindWiring[manifest.HostBindingDTO, binding.Binding]{
		Kind: "HostBinding", Docs: d.bindings, Names: b.idx.Bindings, Rows: b.rows.Bindings,
		To: manifest.ToHostBinding, Meta: func(x *binding.Binding) *meta.Metadata { return &x.Meta },
		Upsert: s.HostBinding.Upsert, Delete: s.HostBinding.Delete,
	}); err != nil {
		return err
	}
	return planKind(ctx, b, kindWiring[manifest.PolicyDTO, policy.Policy]{
		Kind: "Policy", Docs: d.policies, Names: b.idx.Policies, Rows: b.rows.Policies,
		To: manifest.ToPolicy, Meta: func(p *policy.Policy) *meta.Metadata { return &p.Meta },
		Upsert: s.Policy.Upsert, Delete: deletePolicyWithDetach(s),
		Check: refsFor(b, refcheck.Checker.Policy),
	})
}

// deletePolicyWithDetach runs the reference cleanup the control API's delete
// runs: a host listing the policy in its menu stops listing it. Bindings of
// the policy go with it (foreign key).
func deletePolicyWithDetach(s *Stores) func(context.Context, string) error {
	refs := policy.DetachStores{Hosts: s.Host}
	return func(ctx context.Context, id string) error {
		if err := policy.Detach(ctx, refs, id); err != nil {
			return err
		}
		return s.Policy.Delete(ctx, id)
	}
}

// keepHostKeySecret keeps the stored secret of a stored or oauth host key
// whose document carries no value, as an export does; Store.Upsert leaves
// the ciphertext untouched when it sees Resolved and no Value.
func keepHostKeySecret(prev, next *hostkey.HostKey) {
	kind := next.Spec.ValueFrom.Kind
	if next.Spec.Value != "" || kind != prev.Spec.ValueFrom.Kind ||
		(kind != hostkey.ValueKindStored && kind != hostkey.ValueKindOAuth) {
		return
	}
	next.Resolved = prev.Resolved
}
