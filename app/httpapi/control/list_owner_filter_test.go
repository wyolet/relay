package control

import (
	"testing"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/filter"
	"github.com/wyolet/relay/pkg/meta"
)

// ownedRows are three rows, one per scope: a team, a project, and a user.
var ownedRows = []meta.Metadata{
	{Name: "in-team", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: "t1"}},
	{Name: "in-project", Owner: meta.Owner{Kind: meta.OwnerProject, ID: "p1"}},
	{Name: "personal", Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u1"}},
}

func checkOwnerFilter[T any](t *testing.T, s filter.Schema[T], wrap func(meta.Metadata) *T, name func(*T) string) {
	t.Helper()
	items := make([]*T, len(ownedRows))
	for i, m := range ownedRows {
		items[i] = wrap(m)
	}
	cases := []struct {
		q    string
		want []string
	}{
		{"owner=team", []string{"in-team"}},
		{"owner_id=p1", []string{"in-project"}},
		{"owner_id=t1&owner_id=u1", []string{"in-team", "personal"}},
		{"owner=team&owner_id=p1", nil},
	}
	for _, c := range cases {
		got, _ := applyQ(t, s, c.q, items)
		g := make([]string, len(got))
		for i, r := range got {
			g[i] = name(r)
		}
		if !equalSet(g, c.want) {
			t.Errorf("%s => %v, want %v", c.q, g, c.want)
		}
	}
}

func equalSet(a, b []string) bool {
	seen := map[string]bool{}
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			return false
		}
	}
	return len(a) == len(b)
}

func TestOwnerFilters(t *testing.T) {
	t.Run("policies", func(t *testing.T) {
		checkOwnerFilter(t, policyFilter, func(m meta.Metadata) *policy.Policy { return &policy.Policy{Meta: m} },
			func(r *policy.Policy) string { return r.Meta.Name })
	})
	t.Run("pools", func(t *testing.T) {
		checkOwnerFilter(t, poolFilter, func(m meta.Metadata) *pool.Pool { return &pool.Pool{Meta: m} },
			func(r *pool.Pool) string { return r.Meta.Name })
	})
	t.Run("budgets", func(t *testing.T) {
		checkOwnerFilter(t, budgetFilter, func(m meta.Metadata) *budget.Budget { return &budget.Budget{Meta: m} },
			func(r *budget.Budget) string { return r.Meta.Name })
	})
	t.Run("rate-limits", func(t *testing.T) {
		checkOwnerFilter(t, rateLimitFilter, func(m meta.Metadata) *ratelimit.RateLimit { return &ratelimit.RateLimit{Meta: m} },
			func(r *ratelimit.RateLimit) string { return r.Meta.Name })
	})
	t.Run("keys", func(t *testing.T) {
		checkOwnerFilter(t, keyFilter, func(m meta.Metadata) *key.Key { return &key.Key{Meta: m} },
			func(r *key.Key) string { return r.Meta.Name })
	})
	t.Run("host-keys", func(t *testing.T) {
		checkOwnerFilter(t, hostKeyFilter, func(m meta.Metadata) *hostkey.HostKey { return &hostkey.HostKey{Meta: m} },
			func(r *hostkey.HostKey) string { return r.Meta.Name })
	})
	t.Run("service-accounts", func(t *testing.T) {
		checkOwnerFilter(t, serviceAccountFilter, func(m meta.Metadata) *serviceaccount.ServiceAccount {
			return &serviceaccount.ServiceAccount{Meta: m}
		}, func(r *serviceaccount.ServiceAccount) string { return r.Meta.Name })
	})
}
