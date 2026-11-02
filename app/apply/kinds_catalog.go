package apply

import (
	"cmp"
	"context"
	"fmt"
	"slices"

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
		Kind: "Account", Docs: realAccountsFirst(d.accounts), Names: b.idx.Accounts, Rows: b.rows.Accounts,
		To: manifest.ToAccount, Meta: func(a *account.Account) *meta.Metadata { return &a.Meta },
		Upsert: s.Account.Upsert, Delete: s.Account.Delete,
		Check: b.checkAccount(),
		Keep: func(prev, next *account.Account) error {
			if err := keepAccountPlace(prev, next); err != nil {
				return err
			}
			return b.refs().AccountPayloadLogging(ctx, prev, next)
		},
		CheckCreate: refuseCallerAccountCreate,
		CheckDelete: refuseCallerAccountPrune,
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
		Check: b.refs().HostKey, Keep: func(prev, next *hostkey.HostKey) error {
			keepHostKeySecret(prev, next)
			keepAccount(prev, next)
			return nil
		},
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
		// Bindings of a pruned policy go with it (foreign key).
		Upsert: s.Policy.Upsert, Delete: s.Policy.Delete,
		Check: refsFor(b, refcheck.Checker.Policy),
	})
}

// keepAccount keeps the account of a key declared without one, as
// hostkey.Store.Upsert does, so the diff shows what the save writes. A key
// moved to another owner or host leaves its account for the new owner's
// default, which the save fills.
// keepAccountPlace keeps what relay decides about a caller account (its owner, holder, closed mark and kind), so a document edits only its display fields and payload logging; a document cannot give it a host or turn a provider account into one. A share keeps the account it was shared from and its receiver.
func keepAccountPlace(prev, next *account.Account) error {
	if prev.IsProvider() != next.IsProvider() {
		return fmt.Errorf("spec.host cannot be added to or removed from an account; a caller account has none and a provider account keeps its own")
	}
	if prev.Spec.SourceAccountID != next.Spec.SourceAccountID || (prev.IsShared() && prev.Receiver() != next.Receiver()) {
		return fmt.Errorf("spec.sourceAccount and the receiver of a share are fixed: delete the share and share the account again")
	}
	if !prev.IsProvider() {
		next.Meta.Owner, next.Spec.Holder = prev.Meta.Owner, prev.Spec.Holder
		next.Spec.Closed, next.FollowsRoleBindings = prev.Spec.Closed, prev.FollowsRoleBindings
	}
	next.Spec.Kind = next.DerivedKind()
	return nil
}

// realAccountsFirst orders the documents so a real account is planned, and written, before the shares naming it.
func realAccountsFirst(docs []*manifest.AccountDTO) []*manifest.AccountDTO {
	out := slices.Clone(docs)
	slices.SortStableFunc(out, func(a, b *manifest.AccountDTO) int {
		return cmp.Compare(boolRank(a.Spec.SourceAccount != ""), boolRank(b.Spec.SourceAccount != ""))
	})
	return out
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// checkAccount runs the account rules and, for a share this run creates, who may share its real account, as POST /accounts/{ref}/shares does.
func (b *builder) checkAccount() func(context.Context, *account.Account) error {
	check := refsFor(b, refcheck.Checker.Account)
	if check == nil {
		return nil
	}
	stored := make(map[string]bool, len(b.rows.Accounts))
	for _, a := range b.rows.Accounts {
		stored[a.Meta.ID] = true
	}
	return func(ctx context.Context, a *account.Account) error {
		if err := check(ctx, a); err != nil {
			return err
		}
		if !a.IsShared() || stored[a.Meta.ID] {
			return nil
		}
		c := b.refs()
		if src := c.Rows.Account(ctx, a.Spec.SourceAccountID); src != nil {
			return c.MayShare(ctx, src)
		}
		return nil
	}
}

// refuseCallerAccountCreate leaves caller accounts to relay, as the control API does: a document creates provider accounts only.
func refuseCallerAccountCreate(a *account.Account) error {
	if a.IsProvider() {
		return nil
	}
	return fmt.Errorf("spec.host is required: relay keeps the team, project, personal, member and service accounts itself")
}

// refuseCallerAccountPrune deletes a caller account only once it is a closed member account, as the control API does; the rest go with the row they stand for.
func refuseCallerAccountPrune(a *account.Account) error {
	if a.IsProvider() || (a.DerivedKind() == account.KindMember && a.Spec.Closed) {
		return nil
	}
	return fmt.Errorf("relay keeps this account while its %s exists", a.Meta.Owner.Kind)
}

func keepAccount(prev, next *hostkey.HostKey) {
	if next.Spec.AccountID == "" && prev.Spec.HostID == next.Spec.HostID && prev.Meta.Owner == next.Meta.Owner {
		next.Spec.AccountID = prev.Spec.AccountID
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
