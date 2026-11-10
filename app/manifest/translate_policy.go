package manifest

import (
	"fmt"

	catalogmanifest "github.com/wyolet/relay/pkg/catalogkind/manifest"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

// ---------------------------------------------------------------------------
// Policy
// ---------------------------------------------------------------------------

// ToPolicy resolves every linked pool, rate limit and budget by name: the
// catalog translation with relay's pools, budgets and scope owners on top.
func ToPolicy(d PolicyDTO, idx Resolver) (*policy.Policy, error) {
	p, err := catalogmanifest.ToPolicyWith(d, idx, func(d PolicyDTO) ([]string, []policy.BudgetLink, error) {
		return policyLinks(d, idx)
	})
	if err != nil {
		return nil, err
	}
	// The catalog translation resolved a host owner already.
	if p.Meta.Owner.Kind != meta.OwnerHost {
		resolveScopeOwner(&p.Meta.Owner, idx)
	}
	return p, nil
}

func policyLinks(d PolicyDTO, idx Resolver) ([]string, []policy.BudgetLink, error) {
	name := d.Metadata.Name
	pools, err := resolveNames(idx.PoolID, d.Spec.Pools, "policy %q: pool %q not found", name)
	if err != nil {
		return nil, nil, err
	}
	var budgets []policy.BudgetLink
	for i, b := range d.Spec.Budgets {
		if b.Ref == "" {
			return nil, nil, fmt.Errorf("policy %q: budgets[%d].ref is required", name, i)
		}
		id, ok := idx.BudgetID(b.Ref)
		if !ok {
			return nil, nil, refNotFound("policy %q: budgets[%d] budget %q not found", name, i, b.Ref)
		}
		budgets = append(budgets, policy.BudgetLink{BudgetID: id, Shared: b.Shared})
	}
	return pools, budgets, nil
}

// FromPolicy renders a policy; a link counted per account carries shared: false, a shared one leaves it out.
func FromPolicy(p *policy.Policy, rev ReverseResolver) PolicyDTO {
	var limits []PolicyRateLimitDTO
	for _, l := range p.Spec.RateLimits {
		limits = append(limits, PolicyRateLimitDTO{
			Ref:    nameOr(rev.RateLimitName, l.RateLimitID),
			Models: append([]string(nil), l.Models...),
			Shared: eachOnly(l.IsShared()),
		})
	}
	var budgets []PolicyBudgetDTO
	for _, b := range p.Spec.Budgets {
		budgets = append(budgets, PolicyBudgetDTO{Ref: nameOr(rev.BudgetName, b.BudgetID), Shared: eachOnly(b.IsShared())})
	}
	var models []string
	if len(p.Spec.Models) > 0 {
		models = append(models, p.Spec.Models...)
	}
	return PolicyDTO{
		APIVersion: APIVersion,
		Kind:       "Policy",
		Metadata:   metaToWire(p.Meta),
		Spec: PolicySpec{
			Enabled:    p.Spec.Enabled,
			Pools:      namesOf(rev.PoolName, p.Spec.PoolIDs),
			Models:     models,
			RateLimits: limits,
			Budgets:    budgets,
		},
	}
}

// eachOnly renders a link's counting the way a manifest writes it: omitted when shared, the default.
func eachOnly(shared bool) *bool {
	if shared {
		return nil
	}
	return &shared
}

// resolveNames maps each name to its id through lookup; a missing name is a
// RefNotFoundError built from format, which takes the referrer's name and
// then the missing one. A nil or empty input yields nil.
func resolveNames(lookup func(string) (string, bool), names []string, format, referrer string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		id, ok := lookup(n)
		if !ok {
			return nil, refNotFound(format, referrer, n)
		}
		out = append(out, id)
	}
	return out, nil
}

// namesOf renders ids as names; an id the resolver does not know stays as
// is, so a dangling link still shows up in an export.
func namesOf(name func(string) (string, bool), ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, nameOr(name, id))
	}
	return out
}

func nameOr(name func(string) (string, bool), id string) string {
	if n, ok := name(id); ok && n != "" {
		return n
	}
	return id
}

// ---------------------------------------------------------------------------
// RateLimit
// ---------------------------------------------------------------------------

// ToRateLimit converts a RateLimitDTO to a domain RateLimit, resolving a
// team, project, user or host owner from its wire name.
func ToRateLimit(d RateLimitDTO, idx Resolver) (*ratelimit.RateLimit, error) {
	rl, err := catalogmanifest.ToRateLimit(d, idx)
	if err != nil {
		return nil, err
	}
	resolveScopeOwner(&rl.Meta.Owner, idx)
	return rl, nil
}

func FromRateLimit(rl *ratelimit.RateLimit, _ ReverseResolver) RateLimitDTO {
	rules := make([]RateLimitRule, 0, len(rl.Spec.Rules))
	for _, r := range rl.Spec.Rules {
		rules = append(rules, RateLimitRule{
			Meter:    string(r.Meter),
			Amount:   r.Amount,
			Window:   r.Window.Duration().String(),
			Strategy: string(r.Strategy),
		})
	}
	return RateLimitDTO{
		APIVersion: APIVersion,
		Kind:       "RateLimit",
		Metadata:   metaToWire(rl.Meta),
		Spec: RateLimitSpec{
			Rules:   rules,
			Enabled: rl.Spec.Enabled,
		},
	}
}
