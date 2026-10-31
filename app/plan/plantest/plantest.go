// Package plantest builds published plans from rows held in memory, for tests outside app/plan. Builders append rows with fresh ids and unique names; Load runs a real engine over them, and Apply changes a row afterwards the way a write would.
//
// Out of scope: PG, NOTIFY and every fixture beyond the rows a test asks for.
package plantest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"sync/atomic"
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
	"github.com/wyolet/relay/app/plan"
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
	"github.com/wyolet/relay/auth/apikey"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/engine/cell"
	"github.com/wyolet/relay/pkg/slug"
)

// World is the rows a test's plans are built from, one field per kind.
type World struct {
	Providers       []*provider.Provider
	Hosts           []*host.Host
	Models          []*model.Model
	Bindings        []*binding.Binding
	Pricings        []*pricing.Pricing
	Policies        []*policy.Policy
	PolicyBindings  []*policybinding.PolicyBinding
	RateLimits      []*ratelimit.RateLimit
	Budgets         []*budget.Budget
	Pools           []*pool.Pool
	Accounts        []*account.Account
	Credentials     []*hostkey.HostKey
	Teams           []*team.Team
	Projects        []*project.Project
	ServiceAccounts []*serviceaccount.ServiceAccount
	Users           []*user.User
	Keys            []*key.Key
}

var nameSeq atomic.Uint64

func seq() string { return strconv.FormatUint(nameSeq.Add(1), 10) }

// System is the owner of rows nothing narrower owns; system credentials are reached by every principal without a linking policy.
var System = meta.Owner{Kind: meta.OwnerSystem}

func md(name string, owner meta.Owner) meta.Metadata {
	return meta.Metadata{ID: meta.NewID(), Name: name, Owner: owner}
}

// Provider adds a provider.
func (w *World) Provider(name string) *provider.Provider {
	p := &provider.Provider{Meta: md(name, System)}
	w.Providers = append(w.Providers, p)
	return p
}

// Host adds a host serving at baseURL.
func (w *World) Host(name, baseURL string) *host.Host {
	h := &host.Host{Meta: md(name, System), Spec: host.Spec{BaseURL: baseURL}}
	w.Hosts = append(w.Hosts, h)
	return h
}

// Model adds a model of p with one snapshot named like it.
func (w *World) Model(p *provider.Provider, name string) *model.Model {
	m := &model.Model{
		Meta: md(name, meta.Owner{Kind: meta.OwnerProvider, ID: p.Meta.ID}),
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: slug.From(name)}}, Pointer: slug.From(name)},
	}
	w.Models = append(w.Models, m)
	return m
}

// Bind serves m at h through the adapter.
func (w *World) Bind(m *model.Model, h *host.Host, a adapters.Name) *binding.Binding {
	b := &binding.Binding{Meta: md(m.Meta.Name+"-on-"+h.Meta.Name+"-"+seq(), System), Spec: binding.Spec{ModelID: m.Meta.ID, HostID: h.Meta.ID, Adapter: a}}
	w.Bindings = append(w.Bindings, b)
	return b
}

// Price adds a pricing of m at h: perMtok USD per million tokens, input and output alike.
func (w *World) Price(m *model.Model, h *host.Host, perMtok float64) *pricing.Pricing {
	p := &pricing.Pricing{
		Meta: md(m.Meta.Name+"-at-"+h.Meta.Name+"-"+seq(), meta.Owner{Kind: meta.OwnerHost, ID: h.Meta.ID}),
		Spec: pricing.Spec{Currency: "USD", TargetModelIDs: []string{m.Meta.ID}, Rates: []pricing.Rate{
			{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: perMtok},
			{Meter: pricing.MeterTokensOutput, Unit: pricing.UnitPerMillion, Amount: perMtok},
		}},
	}
	w.Pricings = append(w.Pricings, p)
	return p
}

// Account adds a provider account at h.
func (w *World) Account(h *host.Host) *account.Account {
	a := &account.Account{Meta: md("acct-"+seq(), System), Spec: account.Spec{HostID: h.Meta.ID}}
	w.Accounts = append(w.Accounts, a)
	return a
}

// Credential adds a resolved credential at h with value, owned by owner, in account a when non-nil.
func (w *World) Credential(h *host.Host, a *account.Account, owner meta.Owner, value string) *hostkey.HostKey {
	c := &hostkey.HostKey{Meta: md("cred-"+seq(), owner), Spec: hostkey.Spec{HostID: h.Meta.ID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}}}
	if a != nil {
		c.Spec.AccountID = a.Meta.ID
	}
	SetValue(c, value)
	w.Credentials = append(w.Credentials, c)
	return c
}

// SetValue sets a credential's resolved value and its hash the way the store does.
func SetValue(c *hostkey.HostKey, value string) {
	sum := sha256.Sum256([]byte(value))
	c.Resolved, c.KeyHash = value, hex.EncodeToString(sum[:6])
}

// Pool adds a pool at h holding creds in order.
func (w *World) Pool(h *host.Host, creds ...*hostkey.HostKey) *pool.Pool {
	p := &pool.Pool{Meta: md("pool-"+seq(), System), Spec: pool.Spec{HostID: h.Meta.ID}}
	for _, c := range creds {
		p.Spec.CredentialIDs = append(p.Spec.CredentialIDs, c.Meta.ID)
	}
	w.Pools = append(w.Pools, p)
	return p
}

// Rule is a token-bucket rule of amount per window.
func Rule(meter ratelimit.Meter, amount int64, window time.Duration) ratelimit.Rule {
	return ratelimit.Rule{Meter: meter, Amount: amount, Window: ratelimit.Window(window), Strategy: ratelimit.StrategyTokenBucket}
}

// RateLimit adds a rate limit with rules.
func (w *World) RateLimit(rules ...ratelimit.Rule) *ratelimit.RateLimit {
	rl := &ratelimit.RateLimit{Meta: md("rl-"+seq(), System), Spec: ratelimit.Spec{Rules: rules}}
	w.RateLimits = append(w.RateLimits, rl)
	return rl
}

// Budget adds a hard monthly budget of amountUSD.
func (w *World) Budget(amountUSD string) *budget.Budget {
	b := &budget.Budget{Meta: md("budget-"+seq(), System), Spec: budget.Spec{AmountUSD: amountUSD, Period: budget.PeriodMonth, Mode: budget.ModeHard}}
	w.Budgets = append(w.Budgets, b)
	return b
}

// Policy adds a policy with spec.
func (w *World) Policy(name string, spec policy.Spec) *policy.Policy {
	p := &policy.Policy{Meta: md(name+"-"+seq(), System), Spec: spec}
	w.Policies = append(w.Policies, p)
	return p
}

// BindPolicy binds p to subjects.
func (w *World) BindPolicy(p *policy.Policy, subjects ...rolebinding.Subject) *policybinding.PolicyBinding {
	b := &policybinding.PolicyBinding{Meta: md("pb-"+seq(), System), Spec: policybinding.Spec{PolicyID: p.Meta.ID, Subjects: subjects}}
	w.PolicyBindings = append(w.PolicyBindings, b)
	return b
}

// Subject names a binding subject.
func Subject(kind rolebinding.SubjectKind, id string) rolebinding.Subject {
	return rolebinding.Subject{Kind: kind, ID: id}
}

// Team adds a team.
func (w *World) Team() *team.Team {
	t := &team.Team{Meta: md("team-"+seq(), System)}
	w.Teams = append(w.Teams, t)
	return t
}

// Project adds a project of t.
func (w *World) Project(t *team.Team) *project.Project {
	p := &project.Project{Meta: md("proj-"+seq(), meta.Owner{Kind: meta.OwnerTeam, ID: t.Meta.ID}), Spec: project.Spec{TeamID: t.Meta.ID}}
	w.Projects = append(w.Projects, p)
	return p
}

// ServiceAccount adds a service account of p.
func (w *World) ServiceAccount(p *project.Project) *serviceaccount.ServiceAccount {
	s := &serviceaccount.ServiceAccount{Meta: md("sa-"+seq(), meta.Owner{Kind: meta.OwnerProject, ID: p.Meta.ID}), Spec: serviceaccount.Spec{ProjectID: p.Meta.ID}}
	w.ServiceAccounts = append(w.ServiceAccounts, s)
	return s
}

// User adds a user.
func (w *World) User() *user.User {
	u := &user.User{ID: meta.NewID(), Username: "user-" + seq()}
	w.Users = append(w.Users, u)
	return u
}

// UserKey adds a key of u whose bearer is secret.
func (w *World) UserKey(u *user.User, secret string) *key.Key {
	return w.key(key.PrincipalUser, u.ID, secret)
}

// ServiceAccountKey adds a key of s whose bearer is secret.
func (w *World) ServiceAccountKey(s *serviceaccount.ServiceAccount, secret string) *key.Key {
	return w.key(key.PrincipalServiceAccount, s.Meta.ID, secret)
}

func (w *World) key(kind key.PrincipalKind, id, secret string) *key.Key {
	owner := System
	if kind == key.PrincipalUser {
		owner = meta.Owner{Kind: meta.OwnerUser, ID: id}
	}
	k := &key.Key{Meta: md("key-"+seq(), owner), Spec: key.Spec{Principal: key.Principal{Kind: kind, ID: id}, KeyHash: apikey.Hash(secret)}}
	w.Keys = append(w.Keys, k)
	return k
}

// Plans is a loaded engine over a World.
type Plans struct {
	Engine *engine.Engine
	Cells  *cell.Registry
	Holder *plan.Holder
	src    *sources
}

// Load runs an engine over the world's rows and returns it once the first plans are published. The engine closes with the test.
func (w *World) Load(t testing.TB) *Plans {
	t.Helper()
	p := &Plans{Holder: &plan.Holder{}, Cells: cell.NewRegistry(cell.Options{IdleTTL: time.Hour}), src: newSources(w)}
	p.Engine = engine.New(engine.Options{Cells: p.Cells, Hooks: engine.Hooks{OnPublish: p.Holder.OnPublish}})
	plan.Register(p.Engine, p.Cells, p.src.plan())
	if err := p.Engine.Load(context.Background()); err != nil {
		t.Fatalf("plantest: load: %v", err)
	}
	t.Cleanup(func() { _ = p.Engine.Close() })
	return p
}

// Root returns the current published root.
func (p *Plans) Root() *plan.Root { return p.Holder.Load() }

// Apply writes row as a control-API write would: the source serves it from now on and the engine rebuilds what reads it before Apply returns.
func Apply[T any](t testing.TB, p *Plans, row *T) {
	t.Helper()
	upsert(p.src, row)
	if err := engine.Apply(p.Engine, row); err != nil {
		t.Fatalf("plantest: apply: %v", err)
	}
}

// Caller returns the caller plan of the principal a key in w authenticates as.
func (p *Plans) Caller(t testing.TB, k *key.Key) *plan.CallerPlan {
	t.Helper()
	kp := p.Root().Keys.Lookup(k.Spec.KeyHash, time.Now())
	if kp == nil {
		t.Fatalf("plantest: no plan for key %s", k.Meta.Name)
	}
	return kp.Caller
}
