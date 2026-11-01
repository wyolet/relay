package catalogvalidate

import "github.com/wyolet/relay/app/manifest"

// graph indexes docs by kind+name for O(1) ref lookup during checks.
// Slices of issues are intentionally separate from the lookup index so
// checks can be added without touching the indexing code.
type graph struct {
	Providers    map[string]*manifest.ProviderDTO
	Hosts        map[string]*manifest.HostDTO
	Models       map[string]*manifest.ModelDTO
	HostKeys     map[string]*manifest.HostKeyDTO
	Policies     map[string]*manifest.PolicyDTO
	RateLimits   map[string]*manifest.RateLimitDTO
	Keys         map[string]*manifest.KeyDTO
	Pricings     map[string]*manifest.PricingDTO
	HostBindings map[string]*manifest.HostBindingDTO
	Teams        map[string]*manifest.TeamDTO
	Projects     map[string]*manifest.ProjectDTO
	Accounts     map[string]*manifest.AccountDTO
	Pools        map[string]*manifest.PoolDTO
	Budgets      map[string]*manifest.BudgetDTO

	ServiceAccounts map[string]*manifest.ServiceAccountDTO
	Groups          map[string]*manifest.GroupDTO
	Roles           map[string]*manifest.RoleDTO
	RoleBindings    map[string]*manifest.RoleBindingDTO
	PolicyBindings  map[string]*manifest.PolicyBindingDTO

	// duplicates tracks names that appeared more than once within a kind.
	// Each entry holds the kind + name; checkDuplicateNames emits issues.
	duplicates []Ref
}

func buildGraph(docs []manifest.Document) *graph {
	g := &graph{
		Providers:    map[string]*manifest.ProviderDTO{},
		Hosts:        map[string]*manifest.HostDTO{},
		Models:       map[string]*manifest.ModelDTO{},
		HostKeys:     map[string]*manifest.HostKeyDTO{},
		Policies:     map[string]*manifest.PolicyDTO{},
		RateLimits:   map[string]*manifest.RateLimitDTO{},
		Keys:         map[string]*manifest.KeyDTO{},
		Pricings:     map[string]*manifest.PricingDTO{},
		HostBindings: map[string]*manifest.HostBindingDTO{},
		Teams:        map[string]*manifest.TeamDTO{},
		Projects:     map[string]*manifest.ProjectDTO{},
		Accounts:     map[string]*manifest.AccountDTO{},
		Pools:        map[string]*manifest.PoolDTO{},
		Budgets:      map[string]*manifest.BudgetDTO{},

		ServiceAccounts: map[string]*manifest.ServiceAccountDTO{},
		Groups:          map[string]*manifest.GroupDTO{},
		Roles:           map[string]*manifest.RoleDTO{},
		RoleBindings:    map[string]*manifest.RoleBindingDTO{},
		PolicyBindings:  map[string]*manifest.PolicyBindingDTO{},
	}
	for i := range docs {
		d := &docs[i]
		switch {
		case d.Provider != nil:
			indexOne(g, "Provider", d.Provider.Metadata.Name, func() bool {
				_, dup := g.Providers[d.Provider.Metadata.Name]
				g.Providers[d.Provider.Metadata.Name] = d.Provider
				return dup
			})
		case d.Host != nil:
			indexOne(g, "Host", d.Host.Metadata.Name, func() bool {
				_, dup := g.Hosts[d.Host.Metadata.Name]
				g.Hosts[d.Host.Metadata.Name] = d.Host
				return dup
			})
		case d.Model != nil:
			indexOne(g, "Model", d.Model.Metadata.Name, func() bool {
				_, dup := g.Models[d.Model.Metadata.Name]
				g.Models[d.Model.Metadata.Name] = d.Model
				return dup
			})
		case d.HostKey != nil:
			indexOne(g, "HostKey", d.HostKey.Metadata.Name, func() bool {
				_, dup := g.HostKeys[d.HostKey.Metadata.Name]
				g.HostKeys[d.HostKey.Metadata.Name] = d.HostKey
				return dup
			})
		case d.Policy != nil:
			indexOne(g, "Policy", d.Policy.Metadata.Name, func() bool {
				_, dup := g.Policies[d.Policy.Metadata.Name]
				g.Policies[d.Policy.Metadata.Name] = d.Policy
				return dup
			})
		case d.Account != nil:
			indexOne(g, "Account", d.Account.Metadata.Name, func() bool {
				_, dup := g.Accounts[d.Account.Metadata.Name]
				g.Accounts[d.Account.Metadata.Name] = d.Account
				return dup
			})
		case d.Pool != nil:
			indexOne(g, "Pool", d.Pool.Metadata.Name, func() bool {
				_, dup := g.Pools[d.Pool.Metadata.Name]
				g.Pools[d.Pool.Metadata.Name] = d.Pool
				return dup
			})
		case d.Budget != nil:
			indexOne(g, "Budget", d.Budget.Metadata.Name, func() bool {
				_, dup := g.Budgets[d.Budget.Metadata.Name]
				g.Budgets[d.Budget.Metadata.Name] = d.Budget
				return dup
			})
		case d.RateLimit != nil:
			indexOne(g, "RateLimit", d.RateLimit.Metadata.Name, func() bool {
				_, dup := g.RateLimits[d.RateLimit.Metadata.Name]
				g.RateLimits[d.RateLimit.Metadata.Name] = d.RateLimit
				return dup
			})
		case d.Key != nil:
			indexOne(g, "Key", d.Key.Metadata.Name, func() bool {
				_, dup := g.Keys[d.Key.Metadata.Name]
				g.Keys[d.Key.Metadata.Name] = d.Key
				return dup
			})
		case d.Pricing != nil:
			indexOne(g, "Pricing", d.Pricing.Metadata.Name, func() bool {
				_, dup := g.Pricings[d.Pricing.Metadata.Name]
				g.Pricings[d.Pricing.Metadata.Name] = d.Pricing
				return dup
			})
		case d.Team != nil:
			indexOne(g, "Team", d.Team.Metadata.Name, func() bool {
				_, dup := g.Teams[d.Team.Metadata.Name]
				g.Teams[d.Team.Metadata.Name] = d.Team
				return dup
			})
		case d.Project != nil:
			indexOne(g, "Project", d.Project.Metadata.Name, func() bool {
				_, dup := g.Projects[d.Project.Metadata.Name]
				g.Projects[d.Project.Metadata.Name] = d.Project
				return dup
			})
		case d.ServiceAccount != nil:
			indexOne(g, "ServiceAccount", d.ServiceAccount.Metadata.Name, func() bool {
				_, dup := g.ServiceAccounts[d.ServiceAccount.Metadata.Name]
				g.ServiceAccounts[d.ServiceAccount.Metadata.Name] = d.ServiceAccount
				return dup
			})
		case d.Group != nil:
			indexOne(g, "Group", d.Group.Metadata.Name, func() bool {
				_, dup := g.Groups[d.Group.Metadata.Name]
				g.Groups[d.Group.Metadata.Name] = d.Group
				return dup
			})
		case d.Role != nil:
			indexOne(g, "Role", d.Role.Metadata.Name, func() bool {
				_, dup := g.Roles[d.Role.Metadata.Name]
				g.Roles[d.Role.Metadata.Name] = d.Role
				return dup
			})
		case d.RoleBinding != nil:
			indexOne(g, "RoleBinding", d.RoleBinding.Metadata.Name, func() bool {
				_, dup := g.RoleBindings[d.RoleBinding.Metadata.Name]
				g.RoleBindings[d.RoleBinding.Metadata.Name] = d.RoleBinding
				return dup
			})
		case d.PolicyBinding != nil:
			indexOne(g, "PolicyBinding", d.PolicyBinding.Metadata.Name, func() bool {
				_, dup := g.PolicyBindings[d.PolicyBinding.Metadata.Name]
				g.PolicyBindings[d.PolicyBinding.Metadata.Name] = d.PolicyBinding
				return dup
			})
		case d.HostBinding != nil:
			indexOne(g, "HostBinding", d.HostBinding.Metadata.Name, func() bool {
				_, dup := g.HostBindings[d.HostBinding.Metadata.Name]
				g.HostBindings[d.HostBinding.Metadata.Name] = d.HostBinding
				return dup
			})
		}
	}
	return g
}

func indexOne(g *graph, kind, name string, insertReportDup func() bool) {
	if name == "" {
		return
	}
	if insertReportDup() {
		g.duplicates = append(g.duplicates, Ref{Kind: kind, Name: name})
	}
}
