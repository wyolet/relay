package plantest

import (
	"context"
	"sync"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
)

// rows is a mutable in-memory source of one kind.
type rows[T any] struct {
	mu   sync.Mutex
	list []*T
	id   func(*T) string
}

func (r *rows[T]) List(context.Context) ([]*T, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*T(nil), r.list...), nil
}

func (r *rows[T]) Get(_ context.Context, id string) (*T, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.list {
		if r.id(x) == id {
			return x, nil
		}
	}
	return nil, nil
}

func (r *rows[T]) put(row *T) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, x := range r.list {
		if r.id(x) == r.id(row) {
			r.list[i] = row
			return
		}
	}
	r.list = append(r.list, row)
}

func newRows[T any](list []*T, id func(*T) string) *rows[T] {
	return &rows[T]{list: append([]*T(nil), list...), id: id}
}

type sources struct {
	providers   *rows[provider.Provider]
	hosts       *rows[host.Host]
	models      *rows[model.Model]
	bindings    *rows[binding.Binding]
	pricings    *rows[pricing.Pricing]
	policies    *rows[policy.Policy]
	pbs         *rows[policybinding.PolicyBinding]
	rls         *rows[ratelimit.RateLimit]
	budgets     *rows[budget.Budget]
	pools       *rows[pool.Pool]
	accounts    *rows[account.Account]
	credentials *rows[hostkey.HostKey]
	teams       *rows[team.Team]
	projects    *rows[project.Project]
	sas         *rows[serviceaccount.ServiceAccount]
	users       *rows[user.User]
	keys        *rows[key.Key]
}

func newSources(w *World) *sources {
	return &sources{
		providers:   newRows(w.Providers, func(r *provider.Provider) string { return r.Meta.ID }),
		hosts:       newRows(w.Hosts, func(r *host.Host) string { return r.Meta.ID }),
		models:      newRows(w.Models, func(r *model.Model) string { return r.Meta.ID }),
		bindings:    newRows(w.Bindings, func(r *binding.Binding) string { return r.Meta.ID }),
		pricings:    newRows(w.Pricings, func(r *pricing.Pricing) string { return r.Meta.ID }),
		policies:    newRows(w.Policies, func(r *policy.Policy) string { return r.Meta.ID }),
		pbs:         newRows(w.PolicyBindings, func(r *policybinding.PolicyBinding) string { return r.Meta.ID }),
		rls:         newRows(w.RateLimits, func(r *ratelimit.RateLimit) string { return r.Meta.ID }),
		budgets:     newRows(w.Budgets, func(r *budget.Budget) string { return r.Meta.ID }),
		pools:       newRows(w.Pools, func(r *pool.Pool) string { return r.Meta.ID }),
		accounts:    newRows(w.Accounts, func(r *account.Account) string { return r.Meta.ID }),
		credentials: newRows(w.Credentials, func(r *hostkey.HostKey) string { return r.Meta.ID }),
		teams:       newRows(w.Teams, func(r *team.Team) string { return r.Meta.ID }),
		projects:    newRows(w.Projects, func(r *project.Project) string { return r.Meta.ID }),
		sas:         newRows(w.ServiceAccounts, func(r *serviceaccount.ServiceAccount) string { return r.Meta.ID }),
		users:       newRows(w.Users, func(r *user.User) string { return r.ID }),
		keys:        newRows(w.Keys, func(r *key.Key) string { return r.Meta.ID }),
	}
}

func (s *sources) plan() plan.Sources {
	return plan.Sources{
		Providers: s.providers, Hosts: s.hosts, Models: s.models, Bindings: s.bindings, Pricings: s.pricings,
		Policies: s.policies, PolicyBindings: s.pbs, RateLimits: s.rls, Budgets: s.budgets, Pools: s.pools,
		Accounts: s.accounts, Credentials: s.credentials, Teams: s.teams, Projects: s.projects,
		ServiceAccounts: s.sas, Users: s.users, Keys: s.keys,
	}
}

// upsert stores row in the source of its kind, so a later fetch or resync sees it.
func upsert(s *sources, row any) {
	switch r := row.(type) {
	case *provider.Provider:
		s.providers.put(r)
	case *host.Host:
		s.hosts.put(r)
	case *model.Model:
		s.models.put(r)
	case *binding.Binding:
		s.bindings.put(r)
	case *pricing.Pricing:
		s.pricings.put(r)
	case *policy.Policy:
		s.policies.put(r)
	case *policybinding.PolicyBinding:
		s.pbs.put(r)
	case *ratelimit.RateLimit:
		s.rls.put(r)
	case *budget.Budget:
		s.budgets.put(r)
	case *pool.Pool:
		s.pools.put(r)
	case *account.Account:
		s.accounts.put(r)
	case *hostkey.HostKey:
		s.credentials.put(r)
	case *team.Team:
		s.teams.put(r)
	case *project.Project:
		s.projects.put(r)
	case *serviceaccount.ServiceAccount:
		s.sas.put(r)
	case *user.User:
		s.users.put(r)
	case *key.Key:
		s.keys.put(r)
	default:
		panic("plantest: no source for this row type")
	}
}
