package manifest

import (
	"fmt"
	"time"

	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

// ---------------------------------------------------------------------------
// Policy
// ---------------------------------------------------------------------------

// ToPolicy resolves every linked pool, rate limit and budget by name. Model
// refs are canonicalised exactly as the control API does, or apply reports an
// update on every run for a grant the API wrote.
func ToPolicy(d PolicyDTO, idx Resolver) (*policy.Policy, error) {
	name := d.Metadata.Name
	models, err := policy.CanonicalizeModelRefs(d.Spec.Models)
	if err != nil {
		return nil, fmt.Errorf("policy %q: models: %w", name, err)
	}
	pools, err := resolveNames(idx.PoolID, d.Spec.Pools, "policy %q: pool %q not found", name)
	if err != nil {
		return nil, err
	}
	var budgets []policy.BudgetLink
	for i, b := range d.Spec.Budgets {
		if b.Ref == "" {
			return nil, fmt.Errorf("policy %q: budgets[%d].ref is required", name, i)
		}
		id, ok := idx.BudgetID(b.Ref)
		if !ok {
			return nil, refNotFound("policy %q: budgets[%d] budget %q not found", name, i, b.Ref)
		}
		budgets = append(budgets, policy.BudgetLink{BudgetID: id, Shared: b.Shared})
	}
	var limits []policy.RateLimitLink
	for i, l := range d.Spec.RateLimits {
		if l.Ref == "" {
			return nil, fmt.Errorf("policy %q: rateLimits[%d].ref is required", name, i)
		}
		id, ok := idx.RateLimitID(l.Ref)
		if !ok {
			return nil, refNotFound("policy %q: rateLimits[%d] rate limit %q not found", name, i, l.Ref)
		}
		scope, err := policy.CanonicalizeModelRefs(l.Models)
		if err != nil {
			return nil, fmt.Errorf("policy %q: rateLimits[%d].models: %w", name, i, err)
		}
		limits = append(limits, policy.RateLimitLink{RateLimitID: id, Models: scope, Shared: l.Shared})
	}

	m := d.Metadata.toMeta()
	resolveScopeOwner(&m.Owner, idx)
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

// ToRateLimit converts a RateLimitDTO to a domain RateLimit. Resolves
// owner.id from a host *name* to its id when Owner.Kind=host (the wire
// form uses names for human readability).
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
	m := d.Metadata.toMeta()
	if m.Owner.Kind == meta.OwnerHost && m.Owner.ID != "" {
		if hid, ok := idx.HostID(m.Owner.ID); ok {
			m.Owner.ID = hid
		}
	}
	resolveScopeOwner(&m.Owner, idx)
	return &ratelimit.RateLimit{
		Meta: m,
		Spec: ratelimit.Spec{
			Rules:   rules,
			Enabled: d.Spec.Enabled,
		},
	}, nil
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
