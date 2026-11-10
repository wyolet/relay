package manifest

import (
	"fmt"
	"time"

	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
)

// PolicyLinks resolves the links a policy may carry that a catalog tree has no kind for: its pools and budgets.
type PolicyLinks func(d PolicyDTO) (poolIDs []string, budgets []policy.BudgetLink, err error)

// ToPolicy translates a catalog tier policy: model refs, rate limits and a
// host owner. A policy linking pools or budgets is refused; ToPolicyWith
// resolves those.
func ToPolicy(d PolicyDTO, idx Resolver) (*policy.Policy, error) {
	return ToPolicyWith(d, idx, nil)
}

// ToPolicyWith is ToPolicy with links resolving the pools and budgets. Model
// refs are canonicalised exactly as the control API does, or apply reports an
// update on every run for a grant the API wrote. An owner of a kind other than
// host keeps its wire name for the caller to resolve.
func ToPolicyWith(d PolicyDTO, idx Resolver, links PolicyLinks) (*policy.Policy, error) {
	name := d.Metadata.Name
	models, err := policy.CanonicalizeModelRefs(d.Spec.Models)
	if err != nil {
		return nil, fmt.Errorf("policy %q: models: %w", name, err)
	}
	var pools []string
	var budgets []policy.BudgetLink
	switch {
	case links != nil:
		if pools, budgets, err = links(d); err != nil {
			return nil, err
		}
	case len(d.Spec.Pools) > 0 || len(d.Spec.Budgets) > 0:
		return nil, fmt.Errorf("policy %q: pools and budgets are not catalog links", name)
	}
	var limits []policy.RateLimitLink
	for i, l := range d.Spec.RateLimits {
		if l.Ref == "" {
			return nil, fmt.Errorf("policy %q: rateLimits[%d].ref is required", name, i)
		}
		id, ok := idx.RateLimitID(l.Ref)
		if !ok {
			return nil, RefNotFound("policy %q: rateLimits[%d] rate limit %q not found", name, i, l.Ref)
		}
		scope, err := policy.CanonicalizeModelRefs(l.Models)
		if err != nil {
			return nil, fmt.Errorf("policy %q: rateLimits[%d].models: %w", name, i, err)
		}
		limits = append(limits, policy.RateLimitLink{RateLimitID: id, Models: scope, Shared: l.Shared})
	}

	m := d.Metadata.ToMeta()
	resolveHostOwner(&m.Owner, idx)
	p := &policy.Policy{
		Meta: m,
		Spec: policy.Spec{
			Models:     models,
			PoolIDs:    pools,
			RateLimits: limits,
			Budgets:    budgets,
			Enabled:    d.Spec.Enabled,
		},
	}
	p.Normalize()
	return p, nil
}

// ToRateLimit converts a RateLimitDTO to a domain RateLimit. Resolves
// owner.id from a host *name* to its id when Owner.Kind=host (the wire
// form uses names for human readability); an owner of another kind keeps its
// wire name for the caller to resolve.
func ToRateLimit(d RateLimitDTO, idx Resolver) (*ratelimit.RateLimit, error) {
	rules := make([]ratelimit.Rule, 0, len(d.Spec.Rules))
	for i, r := range d.Spec.Rules {
		w, err := parseDuration(r.Window)
		if err != nil {
			return nil, fmt.Errorf("ratelimit %q: rules[%d].window: %w", d.Metadata.Name, i, err)
		}
		rules = append(rules, ratelimit.Rule{
			Meter:    ratelimit.Meter(r.Meter),
			Amount:   r.Amount,
			Window:   ratelimit.Window(w),
			Strategy: ratelimit.Strategy(r.Strategy),
		})
	}
	m := d.Metadata.ToMeta()
	resolveHostOwner(&m.Owner, idx)
	return &ratelimit.RateLimit{
		Meta: m,
		Spec: ratelimit.Spec{
			Rules:   rules,
			Enabled: d.Spec.Enabled,
		},
	}, nil
}

// parseDuration handles a Window field that may be either a human-readable
// string ("30s", "1m") or an int64 nanosecond count.
func parseDuration(v interface{}) (time.Duration, error) {
	if v == nil {
		return 0, fmt.Errorf("window is required")
	}
	switch val := v.(type) {
	case string:
		return time.ParseDuration(val)
	case int:
		return time.Duration(val), nil
	case int64:
		return time.Duration(val), nil
	case float64:
		return time.Duration(int64(val)), nil
	default:
		return 0, fmt.Errorf("unsupported window type %T", v)
	}
}
