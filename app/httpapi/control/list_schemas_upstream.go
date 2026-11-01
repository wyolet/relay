package control

import (
	"time"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/pkg/filter"
)

var accountFilter = filter.Schema[account.Account]{
	Fields: []filter.Field[account.Account]{
		{Name: "name", Kind: filter.String, Sortable: true, Get: func(a *account.Account) string { return a.Meta.Name }},
		{Name: "owner", Kind: filter.String, Get: func(a *account.Account) string { return string(a.Meta.Owner.Kind) }},
		{Name: "host_id", Kind: filter.String, Repeat: true, Get: func(a *account.Account) string { return a.Spec.HostID }},
		{Name: "org_id", Kind: filter.String, Get: func(a *account.Account) string { return a.Spec.OrgID }},
		{Name: "created", Kind: filter.Time, Sortable: true, GetTime: func(a *account.Account) time.Time { return a.Meta.CreatedAt }},
		{Name: "updated", Kind: filter.Time, Sortable: true, GetTime: func(a *account.Account) time.Time { return a.Meta.UpdatedAt }},
	},
	Q:           func(a *account.Account) []string { return []string{a.Meta.Name, a.Meta.DisplayName, a.Spec.OrgID} },
	Labels:      func(a *account.Account) map[string]string { return labelsOf(a.Meta) },
	DefaultSort: "name",
}

var poolFilter = filter.Schema[pool.Pool]{
	Fields: []filter.Field[pool.Pool]{
		{Name: "name", Kind: filter.String, Sortable: true, Get: func(p *pool.Pool) string { return p.Meta.Name }},
		{Name: "owner", Kind: filter.String, Get: func(p *pool.Pool) string { return string(p.Meta.Owner.Kind) }},
		{Name: "host_id", Kind: filter.String, Repeat: true, Get: func(p *pool.Pool) string { return p.Spec.HostID }},
		{Name: "credential_id", Kind: filter.String, Repeat: true, GetMulti: func(p *pool.Pool) []string { return p.Spec.CredentialIDs }},
		{Name: "selection", Kind: filter.String, Enum: []string{"prioritized", "round-robin", "least-recently-used"},
			Get: func(p *pool.Pool) string { return string(p.Spec.Selection) }},
		{Name: "created", Kind: filter.Time, Sortable: true, GetTime: func(p *pool.Pool) time.Time { return p.Meta.CreatedAt }},
		{Name: "updated", Kind: filter.Time, Sortable: true, GetTime: func(p *pool.Pool) time.Time { return p.Meta.UpdatedAt }},
	},
	Q:           func(p *pool.Pool) []string { return []string{p.Meta.Name, p.Meta.DisplayName, p.Meta.Description} },
	Labels:      func(p *pool.Pool) map[string]string { return labelsOf(p.Meta) },
	DefaultSort: "name",
}

var budgetFilter = filter.Schema[budget.Budget]{
	Fields: []filter.Field[budget.Budget]{
		{Name: "name", Kind: filter.String, Sortable: true, Get: func(b *budget.Budget) string { return b.Meta.Name }},
		{Name: "owner", Kind: filter.String, Get: func(b *budget.Budget) string { return string(b.Meta.Owner.Kind) }},
		{Name: "period", Kind: filter.String, Enum: []string{"day", "week", "month"}, Get: func(b *budget.Budget) string { return string(b.Spec.Period) }},
		{Name: "mode", Kind: filter.String, Enum: []string{"hard", "alert"}, Get: func(b *budget.Budget) string { return string(b.Spec.Mode) }},
		{Name: "created", Kind: filter.Time, Sortable: true, GetTime: func(b *budget.Budget) time.Time { return b.Meta.CreatedAt }},
		{Name: "updated", Kind: filter.Time, Sortable: true, GetTime: func(b *budget.Budget) time.Time { return b.Meta.UpdatedAt }},
	},
	Q:           func(b *budget.Budget) []string { return []string{b.Meta.Name, b.Meta.DisplayName, b.Meta.Description} },
	Labels:      func(b *budget.Budget) map[string]string { return labelsOf(b.Meta) },
	DefaultSort: "name",
}
