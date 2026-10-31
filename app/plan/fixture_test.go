package plan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/engine/cell"
	"github.com/wyolet/relay/pkg/slug"
)

// rows is an in-memory Source over a fixed slice.
type rows[T any] struct {
	list []*T
	id   func(*T) string
}

func (r rows[T]) List(context.Context) ([]*T, error) { return r.list, nil }

func (r rows[T]) Get(_ context.Context, id string) (*T, error) {
	for _, x := range r.list {
		if r.id(x) == id {
			return x, nil
		}
	}
	return nil, nil
}

func src[T any](list []*T, md func(*T) meta.Metadata) rows[T] {
	return rows[T]{list: list, id: func(x *T) string { return md(x).ID }}
}

// world is the rows one test starts the engine with; its builders append and return them.
type world struct {
	providers []*provider.Provider
	hosts     []*host.Host
	models    []*model.Model
	bindings  []*binding.Binding
	pricings  []*pricing.Pricing
	policies  []*policy.Policy
	pbs       []*policybinding.PolicyBinding
	rls       []*ratelimit.RateLimit
	budgets   []*budget.Budget
	pools     []*pool.Pool
	accounts  []*account.Account
	creds     []*hostkey.HostKey
	teams     []*team.Team
	projects  []*project.Project
	sas       []*serviceaccount.ServiceAccount
	users     []*user.User
	keys      []*key.Key
}

func md(name string, owner meta.Owner) meta.Metadata {
	return meta.Metadata{ID: meta.NewID(), Name: name, Owner: owner}
}

var system = meta.Owner{Kind: meta.OwnerSystem}

func (w *world) provider(name string) *provider.Provider {
	p := &provider.Provider{Meta: md(name, system)}
	w.providers = append(w.providers, p)
	return p
}

func (w *world) host(name string) *host.Host {
	h := &host.Host{Meta: md(name, system), Spec: host.Spec{BaseURL: "http://" + name + ".example"}}
	w.hosts = append(w.hosts, h)
	return h
}

func (w *world) model(p *provider.Provider, name string) *model.Model {
	m := &model.Model{
		Meta: md(name, meta.Owner{Kind: meta.OwnerProvider, ID: p.Meta.ID}),
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: name}}, Pointer: slug.From(name)},
	}
	w.models = append(w.models, m)
	return m
}

func (w *world) bind(m *model.Model, h *host.Host) *binding.Binding {
	b := &binding.Binding{Meta: md(m.Meta.Name+"-on-"+h.Meta.Name, system), Spec: binding.Spec{ModelID: m.Meta.ID, HostID: h.Meta.ID, Adapter: adapters.OpenAI}}
	w.bindings = append(w.bindings, b)
	return b
}

func (w *world) price(m *model.Model, h *host.Host) *pricing.Pricing {
	p := &pricing.Pricing{
		Meta: md(m.Meta.Name+"-at-"+h.Meta.Name, meta.Owner{Kind: meta.OwnerHost, ID: h.Meta.ID}),
		Spec: pricing.Spec{Currency: "USD", TargetModelIDs: []string{m.Meta.ID}, Rates: []pricing.Rate{{Meter: "tokens.input", Unit: "per_million", Amount: 1}}},
	}
	w.pricings = append(w.pricings, p)
	return p
}

func (w *world) account(h *host.Host) *account.Account {
	a := &account.Account{Meta: md("acct-"+meta.NewID()[:8], system), Spec: account.Spec{HostID: h.Meta.ID}}
	w.accounts = append(w.accounts, a)
	return a
}

// cred is a resolved credential at h, in account a when non-nil.
func (w *world) cred(h *host.Host, a *account.Account, owner meta.Owner) *hostkey.HostKey {
	c := &hostkey.HostKey{Meta: md("cred-"+meta.NewID()[:8], owner), Spec: hostkey.Spec{HostID: h.Meta.ID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}}}
	if a != nil {
		c.Spec.AccountID = a.Meta.ID
	}
	rotate(c, "sk-"+c.Meta.ID)
	w.creds = append(w.creds, c)
	return c
}

// rotate sets a credential's value and its hash the way the store does.
func rotate(c *hostkey.HostKey, value string) {
	sum := sha256.Sum256([]byte(value))
	c.Resolved, c.KeyHash = value, hex.EncodeToString(sum[:6])
}

func (w *world) pool(h *host.Host, creds ...*hostkey.HostKey) *pool.Pool {
	p := &pool.Pool{Meta: md("pool-"+meta.NewID()[:8], system), Spec: pool.Spec{HostID: h.Meta.ID}}
	for _, c := range creds {
		p.Spec.CredentialIDs = append(p.Spec.CredentialIDs, c.Meta.ID)
	}
	w.pools = append(w.pools, p)
	return p
}

func rule(meter ratelimit.Meter, amount int64, window time.Duration) ratelimit.Rule {
	return ratelimit.Rule{Meter: meter, Amount: amount, Window: ratelimit.Window(window), Strategy: ratelimit.StrategyTokenBucket}
}

func (w *world) rateLimit(rules ...ratelimit.Rule) *ratelimit.RateLimit {
	rl := &ratelimit.RateLimit{Meta: md("rl-"+meta.NewID()[:8], system), Spec: ratelimit.Spec{Rules: rules}}
	w.rls = append(w.rls, rl)
	return rl
}

func (w *world) budget(amount string) *budget.Budget {
	b := &budget.Budget{Meta: md("budget-"+meta.NewID()[:8], system), Spec: budget.Spec{AmountUSD: amount, Period: budget.PeriodMonth, Mode: budget.ModeHard}}
	w.budgets = append(w.budgets, b)
	return b
}

func (w *world) policy(name string, spec policy.Spec) *policy.Policy {
	p := &policy.Policy{Meta: md(name, system), Spec: spec}
	w.policies = append(w.policies, p)
	return p
}

func (w *world) bindPolicy(p *policy.Policy, subjects ...rolebinding.Subject) *policybinding.PolicyBinding {
	b := &policybinding.PolicyBinding{Meta: md("pb-"+meta.NewID()[:8], system), Spec: policybinding.Spec{PolicyID: p.Meta.ID, Subjects: subjects}}
	w.pbs = append(w.pbs, b)
	return b
}

func subject(kind rolebinding.SubjectKind, id string) rolebinding.Subject {
	return rolebinding.Subject{Kind: kind, ID: id}
}

func (w *world) team() *team.Team {
	t := &team.Team{Meta: md("team-"+meta.NewID()[:8], system)}
	w.teams = append(w.teams, t)
	return t
}

func (w *world) project(t *team.Team) *project.Project {
	p := &project.Project{Meta: md("proj-"+meta.NewID()[:8], meta.Owner{Kind: meta.OwnerTeam, ID: t.Meta.ID}), Spec: project.Spec{TeamID: t.Meta.ID}}
	w.projects = append(w.projects, p)
	return p
}

func (w *world) sa(p *project.Project) *serviceaccount.ServiceAccount {
	s := &serviceaccount.ServiceAccount{Meta: md("sa-"+meta.NewID()[:8], meta.Owner{Kind: meta.OwnerProject, ID: p.Meta.ID}), Spec: serviceaccount.Spec{ProjectID: p.Meta.ID}}
	w.sas = append(w.sas, s)
	return s
}

func (w *world) user() *user.User {
	u := &user.User{ID: meta.NewID(), Username: "user-" + meta.NewID()[:8]}
	w.users = append(w.users, u)
	return u
}

func keyHash(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

func (w *world) keyFor(kind key.PrincipalKind, id string) *key.Key {
	k := &key.Key{Meta: md("key-"+meta.NewID()[:8], system), Spec: key.Spec{Principal: key.Principal{Kind: kind, ID: id}}}
	k.Spec.KeyHash = keyHash(k.Meta.ID)
	w.keys = append(w.keys, k)
	return k
}

func (w *world) saKey(s *serviceaccount.ServiceAccount) *key.Key {
	return w.keyFor(key.PrincipalServiceAccount, s.Meta.ID)
}

func (w *world) userKey(u *user.User) *key.Key { return w.keyFor(key.PrincipalUser, u.ID) }

func (w *world) sources() Sources {
	return Sources{
		Providers:       src(w.providers, func(r *provider.Provider) meta.Metadata { return r.Meta }),
		Hosts:           src(w.hosts, func(r *host.Host) meta.Metadata { return r.Meta }),
		Models:          src(w.models, func(r *model.Model) meta.Metadata { return r.Meta }),
		Bindings:        src(w.bindings, func(r *binding.Binding) meta.Metadata { return r.Meta }),
		Pricings:        src(w.pricings, func(r *pricing.Pricing) meta.Metadata { return r.Meta }),
		Policies:        src(w.policies, func(r *policy.Policy) meta.Metadata { return r.Meta }),
		PolicyBindings:  src(w.pbs, func(r *policybinding.PolicyBinding) meta.Metadata { return r.Meta }),
		RateLimits:      src(w.rls, func(r *ratelimit.RateLimit) meta.Metadata { return r.Meta }),
		Budgets:         src(w.budgets, func(r *budget.Budget) meta.Metadata { return r.Meta }),
		Pools:           src(w.pools, func(r *pool.Pool) meta.Metadata { return r.Meta }),
		Accounts:        src(w.accounts, func(r *account.Account) meta.Metadata { return r.Meta }),
		Credentials:     src(w.creds, func(r *hostkey.HostKey) meta.Metadata { return r.Meta }),
		Teams:           src(w.teams, func(r *team.Team) meta.Metadata { return r.Meta }),
		Projects:        src(w.projects, func(r *project.Project) meta.Metadata { return r.Meta }),
		ServiceAccounts: src(w.sas, func(r *serviceaccount.ServiceAccount) meta.Metadata { return r.Meta }),
		Users:           rows[user.User]{list: w.users, id: func(u *user.User) string { return u.ID }},
		Keys:            src(w.keys, func(r *key.Key) meta.Metadata { return r.Meta }),
	}
}

// harness is a loaded engine over a world.
type harness struct {
	t      testing.TB
	e      *engine.Engine
	cells  *cell.Registry
	holder *Holder
}

func (w *world) start(t testing.TB) *harness {
	t.Helper()
	h := &harness{t: t, cells: cell.NewRegistry(cell.Options{IdleTTL: time.Minute}), holder: &Holder{}}
	h.e = engine.New(engine.Options{Cells: h.cells, Hooks: engine.Hooks{OnPublish: h.holder.OnPublish}})
	Register(h.e, h.cells, w.sources())
	if err := h.e.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Cleanup(func() { _ = h.e.Close() })
	return h
}

func apply[T any](h *harness, row *T) {
	h.t.Helper()
	if err := engine.Apply(h.e, row); err != nil {
		h.t.Fatalf("apply: %v", err)
	}
}

func del(h *harness, k engine.Kind, id string) {
	h.t.Helper()
	if err := engine.Delete(h.e, engine.Ref{Kind: k, ID: id}); err != nil {
		h.t.Fatalf("delete: %v", err)
	}
}

// keyPlan is what authentication finds for k now.
func (h *harness) keyPlan(k *key.Key) *KeyPlan {
	return h.holder.Load().Keys.Lookup(k.Spec.KeyHash, time.Now())
}

func (h *harness) route(k *key.Key, ref string) *Route {
	h.t.Helper()
	kp := h.keyPlan(k)
	if kp == nil {
		h.t.Fatalf("no key plan for %s", k.Meta.Name)
	}
	return kp.Caller.Routes.Get(ref)
}

func get[T any](h *harness, id string) *T { return engine.Get[T](h.e.View(), id) }

func (h *harness) issues(k engine.Kind, id string) []engine.Issue {
	return h.e.View().Issues(engine.Ref{Kind: k, ID: id})
}

func hasIssue(is []engine.Issue, code string) bool {
	for _, i := range is {
		if i.Code == code {
			return true
		}
	}
	return false
}

// slotIDs lists a route's credentials hop by hop.
func slotIDs(r *Route) [][]string {
	var out [][]string
	for _, hop := range r.Hops {
		var ids []string
		for _, s := range hop.Slots {
			ids = append(ids, s.Credential.Meta.ID)
		}
		out = append(out, ids)
	}
	return out
}

func ids(cs ...*hostkey.HostKey) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Meta.ID
	}
	return out
}

func clone[T any](x *T) *T { c := *x; return &c }

// basic is one provider, two hosts serving model m1, an account per host and two credentials each, all system-owned.
type basic struct {
	w            *world
	prov         *provider.Provider
	hostA, hostB *host.Host
	m1           *model.Model
	acctA, acctB *account.Account
	a1, a2, b1   *hostkey.HostKey
	team         *team.Team
	proj         *project.Project
	sa           *serviceaccount.ServiceAccount
	key          *key.Key
}

func newBasic() *basic {
	w := &world{}
	b := &basic{w: w}
	b.prov = w.provider("acme")
	b.hostA, b.hostB = w.host("host-a"), w.host("host-b")
	b.m1 = w.model(b.prov, "m1")
	w.bind(b.m1, b.hostA)
	w.bind(b.m1, b.hostB)
	w.price(b.m1, b.hostA)
	w.price(b.m1, b.hostB)
	b.acctA, b.acctB = w.account(b.hostA), w.account(b.hostB)
	b.a1 = w.cred(b.hostA, b.acctA, system)
	b.a2 = w.cred(b.hostA, b.acctA, system)
	b.b1 = w.cred(b.hostB, b.acctB, system)
	b.team = w.team()
	b.proj = w.project(b.team)
	b.sa = w.sa(b.proj)
	b.key = w.saKey(b.sa)
	return b
}
