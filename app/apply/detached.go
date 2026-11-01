package apply

import (
	"context"
	"slices"
	"sort"

	"github.com/wyolet/relay/app/policy"
)

// DetachedLinks lists the policies a prune takes a link from. The prune is
// allowed: a policy stops counting a deleted rate limit or budget, and
// narrows to the pools and credentials it still links, never widening.
type DetachedLinks struct {
	// Field is the policy field that loses the link.
	Field string `json:"field"`
	// Policies names the policies the caller may see.
	Policies []string `json:"policies,omitempty"`
	// Hidden counts the policies the caller may not see.
	Hidden int `json:"hidden,omitempty"`
}

// linkedBy maps a prunable kind to the policy field linking it and the test
// for whether a policy links an id.
var linkedBy = map[string]struct {
	field string
	links func(p *policy.Policy, id string) bool
}{
	"HostKey": {"spec.credentials", func(p *policy.Policy, id string) bool { return slices.Contains(p.Spec.CredentialIDs, id) }},
	"Pool":    {"spec.pools", func(p *policy.Policy, id string) bool { return slices.Contains(p.Spec.PoolIDs, id) }},
	"Budget":  {"spec.budgets", func(p *policy.Policy, id string) bool { return slices.Contains(p.Spec.BudgetIDs, id) }},
	"RateLimit": {"spec.rateLimits", func(p *policy.Policy, id string) bool {
		return slices.ContainsFunc(p.Spec.RateLimits, func(l policy.RateLimitLink) bool { return l.RateLimitID == id })
	}},
}

// reportDetached fills Detached on every planned delete of a linked kind,
// against the policies as they stand after this run: the bundle's version
// of a declared policy, the stored one otherwise, and none for a pruned one.
func (b *builder) reportDetached(ctx context.Context) {
	pruned := map[string]bool{}
	for _, group := range b.deletes {
		for _, e := range group {
			if e.Kind == "Policy" {
				pruned[e.ID] = true
			}
		}
	}
	var after []*policy.Policy
	for _, stored := range b.rows.Policies {
		id := stored.Meta.ID
		if pruned[id] {
			continue
		}
		if p, ok := b.declared[id].(*policy.Policy); ok {
			after = append(after, p)
			continue
		}
		after = append(after, stored)
	}
	for id, row := range b.declared {
		if p, ok := row.(*policy.Policy); ok && !slices.ContainsFunc(after, func(q *policy.Policy) bool { return q.Meta.ID == id }) {
			after = append(after, p)
		}
	}

	for _, group := range b.deletes {
		for i := range group {
			e := &group[i]
			link, ok := linkedBy[e.Kind]
			if !ok {
				continue
			}
			d := &DetachedLinks{Field: link.field}
			for _, p := range after {
				if !link.links(p, e.ID) {
					continue
				}
				if b.visible(ctx, "Policy", p.Meta.ID, p.Meta.Owner) {
					d.Policies = append(d.Policies, p.Meta.Name)
				} else {
					d.Hidden++
				}
			}
			if len(d.Policies) > 0 || d.Hidden > 0 {
				sort.Strings(d.Policies)
				e.Detached = d
			}
		}
	}
}
