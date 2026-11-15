package catalogview

import (
	"context"
	"time"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

type (
	fModels    []*model.Model
	fHosts     []*host.Host
	fBindings  []*binding.Binding
	fPricings  []*pricing.Pricing
	fPolicies  []*policy.Policy
	fRLs       []*ratelimit.RateLimit
	fProviders []*provider.Provider
	fHostKeys  []*hostkey.HostKey
	fPools     []*pool.Pool
	fAccounts  []*account.Account
)

func (f fModels) List(context.Context) ([]*model.Model, error)          { return f, nil }
func (f fHosts) List(context.Context) ([]*host.Host, error)             { return f, nil }
func (f fBindings) List(context.Context) ([]*binding.Binding, error)    { return f, nil }
func (f fPricings) List(context.Context) ([]*pricing.Pricing, error)    { return f, nil }
func (f fPolicies) List(context.Context) ([]*policy.Policy, error)      { return f, nil }
func (f fRLs) List(context.Context) ([]*ratelimit.RateLimit, error)     { return f, nil }
func (f fProviders) List(context.Context) ([]*provider.Provider, error) { return f, nil }
func (f fHostKeys) List(context.Context) ([]*hostkey.HostKey, error)    { return f, nil }
func (f fPools) List(context.Context) ([]*pool.Pool, error)             { return f, nil }
func (f fAccounts) List(context.Context) ([]*account.Account, error)    { return f, nil }

// newService holds rows in the order given. Every id is the row's slug behind
// its kind ("host:azure"), so a wanted view reads off the names. Pools and
// Accounts stay nil, the optional stores unset, until a row of theirs is given.
func newService(rows ...any) *Service {
	s := &Service{Models: fModels{}, Hosts: fHosts{}, Bindings: fBindings{}, Pricings: fPricings{}, Policies: fPolicies{}, RateLimits: fRLs{}, Providers: fProviders{}, HostKeys: fHostKeys{}}
	for _, r := range rows {
		switch r := r.(type) {
		case *model.Model:
			s.Models = append(s.Models.(fModels), r)
		case *host.Host:
			s.Hosts = append(s.Hosts.(fHosts), r)
		case *binding.Binding:
			s.Bindings = append(s.Bindings.(fBindings), r)
		case *pricing.Pricing:
			s.Pricings = append(s.Pricings.(fPricings), r)
		case *policy.Policy:
			s.Policies = append(s.Policies.(fPolicies), r)
		case *ratelimit.RateLimit:
			s.RateLimits = append(s.RateLimits.(fRLs), r)
		case *provider.Provider:
			s.Providers = append(s.Providers.(fProviders), r)
		case *hostkey.HostKey:
			s.HostKeys = append(s.HostKeys.(fHostKeys), r)
		case *pool.Pool:
			pools, _ := s.Pools.(fPools)
			s.Pools = append(pools, r)
		case *account.Account:
			accounts, _ := s.Accounts.(fAccounts)
			s.Accounts = append(accounts, r)
		default:
			panic("newService: unknown row")
		}
	}
	return s
}

var (
	operator = meta.Owner{Kind: meta.OwnerUser} // a user row with no owner id
	alice    = meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}
	bob      = meta.Owner{Kind: meta.OwnerUser, ID: "u-bob"}
)

// asAlice sees non-user rows and her own; bob's and operator rows are hidden.
func asAlice(_, _ string, owner meta.Owner) bool {
	return owner.Kind != meta.OwnerUser || owner == alice
}

func hostTier(hostName string) meta.Owner {
	return meta.Owner{Kind: meta.OwnerHost, ID: "host:" + hostName}
}

func provOf(name string) *provider.Provider {
	return &provider.Provider{Meta: meta.Metadata{ID: "prov:" + name, Name: name}}
}

func hostOf(name string) *host.Host {
	return &host.Host{Meta: meta.Metadata{ID: "host:" + name, Name: name}}
}

// modelOf is a model of provider openai.
func modelOf(name string) *model.Model {
	return &model.Model{Meta: meta.Metadata{ID: "model:" + name, Name: name, Owner: meta.Owner{Kind: meta.OwnerProvider, ID: "prov:openai"}}}
}

func bindOf(modelName, hostName string) *binding.Binding {
	return &binding.Binding{
		Meta: meta.Metadata{ID: "bind:" + modelName + "@" + hostName, Name: modelName + "-on-" + hostName},
		Spec: binding.Spec{ModelID: "model:" + modelName, HostID: "host:" + hostName, Adapter: adapters.OpenAI},
	}
}

// sheetOf is hostName's rate sheet covering the named models at amount per million input tokens.
func sheetOf(name, hostName string, amount float64, models ...string) *pricing.Pricing {
	p := &pricing.Pricing{
		Meta: meta.Metadata{ID: "pricing:" + name, Name: name, Owner: hostTier(hostName)},
		Spec: pricing.Spec{Currency: "USD", Rates: []pricing.Rate{{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: amount}}},
	}
	for _, m := range models {
		p.Spec.TargetModelIDs = append(p.Spec.TargetModelIDs, "model:"+m)
	}
	return p
}

func keyOf(name, hostName string, owner meta.Owner) *hostkey.HostKey {
	return &hostkey.HostKey{Meta: meta.Metadata{ID: "key:" + name, Name: name, Owner: owner}, Spec: hostkey.Spec{HostID: "host:" + hostName}}
}

// inAccount places k in the named account.
func inAccount(k *hostkey.HostKey, accountName string) *hostkey.HostKey {
	k.Spec.AccountID = "account:" + accountName
	return k
}

// poolOf is a pool at hostName holding the named keys in order.
func poolOf(name, hostName string, owner meta.Owner, keys ...string) *pool.Pool {
	p := &pool.Pool{Meta: meta.Metadata{ID: "pool:" + name, Name: name, Owner: owner}, Spec: pool.Spec{HostID: "host:" + hostName}}
	for _, k := range keys {
		p.Spec.CredentialIDs = append(p.Spec.CredentialIDs, "key:"+k)
	}
	return p
}

// drawingOn makes p draw on the named accounts whole.
func drawingOn(p *pool.Pool, accounts ...string) *pool.Pool {
	for _, a := range accounts {
		p.Spec.AccountIDs = append(p.Spec.AccountIDs, "account:"+a)
	}
	return p
}

// accountOf is a provider account at hostName; a non-empty source makes it a share of that account.
func accountOf(name, hostName string, owner meta.Owner, source string) *account.Account {
	a := &account.Account{Meta: meta.Metadata{ID: "account:" + name, Name: name, Owner: owner}, Spec: account.Spec{HostID: "host:" + hostName}}
	if source != "" {
		a.Spec.SourceAccountID = "account:" + source
	}
	return a
}

// policyOf allows the model entries through the named pools.
func policyOf(name string, owner meta.Owner, models []string, pools ...string) *policy.Policy {
	p := &policy.Policy{Meta: meta.Metadata{ID: "policy:" + name, Name: name, Owner: owner}, Spec: policy.Spec{Models: models}}
	for _, pl := range pools {
		p.Spec.PoolIDs = append(p.Spec.PoolIDs, "pool:"+pl)
	}
	return p
}

// limitedBy links the named rate limit to p, scoped to models when given.
func limitedBy(p *policy.Policy, rateLimit string, models ...string) *policy.Policy {
	p.Spec.RateLimits = append(p.Spec.RateLimits, policy.RateLimitLink{RateLimitID: "rl:" + rateLimit, Models: models})
	return p
}

// rlOf allows amount requests a minute.
func rlOf(name string, amount int64) *ratelimit.RateLimit {
	return &ratelimit.RateLimit{
		Meta: meta.Metadata{ID: "rl:" + name, Name: name},
		Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{Meter: ratelimit.MeterRequests, Amount: amount, Window: ratelimit.Window(time.Minute), Strategy: ratelimit.StrategyTokenBucket}}},
	}
}

// The wanted views of the rows above.

func hostRef(name string) HostRef { return HostRef{ID: "host:" + name, Name: name, Enabled: true} }

func modelRef(name string) ModelRef { return ModelRef{ID: "model:" + name, Name: name} }

func bindView(modelName, hostName string) BindingView {
	return BindingView{ID: "bind:" + modelName + "@" + hostName, Adapter: "openai", Enabled: true}
}

func sheetView(name string, amount float64) *PricingView {
	return &PricingView{ID: "pricing:" + name, Name: name, Currency: "USD", Rates: []Rate{{Meter: "tokens.input", Unit: "per_million", Amount: amount}}}
}

func limit(amount int64) Limit {
	return Limit{Meter: "requests", Amount: amount, Window: "1m0s", Strategy: "token-bucket"}
}

func policyRef(name string, owner OwnerRef) PolicyRef {
	return PolicyRef{ID: "policy:" + name, Name: name, Owner: owner, Enabled: true}
}

func keyRef(name string) HostKeyRef { return HostKeyRef{ID: "key:" + name, Name: name, Enabled: true} }

func keyView(name string) HostKeyView {
	return HostKeyView{ID: "key:" + name, Name: name, Enabled: true}
}

// tier1 is one model on one priced host, allowed by an operator policy with one rate limit.
func tier1() *Service {
	h := hostOf("openai")
	h.Meta.DisplayName, h.Spec.BaseURL = "OpenAI", "https://api.openai.com"
	b := bindOf("gpt-4o", "openai")
	b.Spec.PricingID = "pricing:openai-gpt-4o"
	return newService(
		provOf("openai"), h, modelOf("gpt-4o"), b,
		sheetOf("openai-gpt-4o", "openai", 2.5, "gpt-4o"),
		limitedBy(policyOf("tier-1", operator, []string{"openai/gpt-4o"}), "rpm"),
		rlOf("rpm", 100),
	)
}

func tier1Host() HostRef {
	return HostRef{ID: "host:openai", Name: "openai", DisplayName: "OpenAI", BaseURL: "https://api.openai.com", Enabled: true}
}
