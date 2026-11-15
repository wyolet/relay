package refcheck

import (
	"testing"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

// A link the save keeps rather than makes passes the caller's visibility check, so a tenant can save a row an admin linked to an organization row; the owner rules still hold.
func TestHeldLinks(t *testing.T) {
	rows := newLinkRows()
	org := meta.Owner{Kind: meta.OwnerOrganization}
	rows.limits["rl-org"] = &ratelimit.RateLimit{Meta: meta.Metadata{ID: "rl-org", Name: "rl-org", Owner: org}}
	rows.budgets["budget-org"] = &budget.Budget{Meta: meta.Metadata{ID: "budget-org", Name: "budget-org", Owner: org}}
	hidden := []string{"rl-org", "budget-org", "pool-p-2"}
	ctx := as("u-alice")
	mk := func(owner meta.Owner, pools []string, limits ...string) *policy.Policy {
		p := &policy.Policy{Meta: meta.Metadata{ID: "pol", Name: "pol", Owner: owner}, Spec: policy.Spec{PoolIDs: pools}}
		for _, id := range limits {
			p.Spec.RateLimits = append(p.Spec.RateLimits, policy.RateLimitLink{RateLimitID: id})
		}
		p.Spec.Budgets = []policy.BudgetLink{{BudgetID: "budget-org"}}
		return p
	}
	stored := mk(teamOne, nil, "rl-org")
	for _, tc := range []struct {
		name string
		held map[string]bool
		p    *policy.Policy
		want string
	}{
		{"nothing held", nil, stored, `404: rate-limit "rl-org" not found`},
		{"the stored row's links", PolicyLinks(stored), stored, ""},
		{"a new hidden link", PolicyLinks(stored), mk(teamOne, []string{"pool-p-2"}, "rl-org"), `404: pool "pool-p-2" not found`},
		{"a held link out of the owner's reach", map[string]bool{"pool-p-2": true, "rl-org": true, "budget-org": true}, mk(projectOne, []string{"pool-p-2"}, "rl-org"),
			`400: a policy of project "p-1" cannot use a pool you cannot see: a project's policy can use pools of the project, its team or the system. Create the pool in the project or its team, or move this one there.`},
	} {
		c := rows.checker(hiding(hidden...))
		c.Held = tc.held
		wantRefused(t, tc.name, c.Policy(ctx, tc.p), tc.want)
	}
}
