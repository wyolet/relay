// The limits pseudo kind: a budget or a rate limit made in a team's, project's or service account's scope and linked from the policy over its account by the policy step.

package pseudo

import (
	"slices"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

// Limits is the body of POST /pseudo-limits.
type Limits struct {
	Metadata LimitsMetadata `json:"metadata,omitempty" doc:"The budget's or rate limit's metadata; its owner is the target's scope."`
	Spec     LimitsSpec     `json:"spec"               doc:"The target and the limit."`
}

// LimitsMetadata is the budget's or rate limit's metadata.
type LimitsMetadata struct {
	Name        string            `json:"name,omitempty"        doc:"Slug of the budget or rate limit. Omitted: derived from displayName, suffixed -2, -3, … while taken."`
	DisplayName string            `json:"displayName,omitempty" doc:"Free-text name shown in the UI."`
	Description string            `json:"description,omitempty" doc:"Free-text description."`
	Labels      map[string]string `json:"labels,omitempty"      doc:"Labels of the budget or rate limit."`
}

// LimitsSpec names the target and the one limit to put on it.
type LimitsSpec struct {
	Target    Target          `json:"target"              doc:"The team, project or service account whose account the limit applies to."`
	Budget    *budget.Spec    `json:"budget,omitempty"    doc:"A spend cap, as POST /budgets takes its spec. Exactly one of budget and rateLimit; both omitted, with dryRun=true, answers only case and current."`
	RateLimit *ratelimit.Spec `json:"rateLimit,omitempty" doc:"A rate limit, as POST /rate-limits takes its spec. Exactly one of budget and rateLimit."`
	Link      *LimitLink      `json:"link,omitempty"      doc:"How the policy links the limit. Omitted: counted once for everyone under the target, over every model."`
}

// LimitLink is how the policy links the limit: the fields of its budgets or rateLimits entry.
type LimitLink struct {
	Shared *bool    `json:"shared,omitempty" doc:"True or omitted counts the limit once for everyone under the target; false counts it separately for each account under it (each lane its own counter). Stored as the link's shared field."`
	Models []string `json:"models,omitempty" doc:"With a rate limit only: the models it applies to, in the policy model grammar (provider or provider/model), checked as a policy update checks a rate-limit link's models and stored in their canonical form. Empty or omitted applies it to every model. With a budget, 400: a budget link has no model scope."`
}

// LimitsReads is what expanding a limits body reads besides it.
type LimitsReads struct {
	Policy PolicyReads
}

// LimitsChange is what a limits body expands to: the policy step and the limit made.
type LimitsChange struct {
	PolicyChange
	// Limit is the budget's or rate limit's row.
	Limit Row
}

// ExpandLimits returns the steps that put the limit on the target: the Budget or RateLimit owned by the target's scope, then the policy step adding its link to what the policy links already.
func ExpandLimits(body Limits, in PolicyReads) (LimitsChange, error) {
	if err := CheckLimits(body); err != nil {
		return LimitsChange{}, err
	}
	s := body.Spec
	if in.Case() == CaseAbove {
		c, err := ChangePolicy(in, func(*policy.Spec) {}, "/spec/target")
		return LimitsChange{PolicyChange: c}, err
	}
	m := meta.Metadata{ID: in.NewID(), Name: body.Metadata.Name, DisplayName: body.Metadata.DisplayName, Description: body.Metadata.Description, Labels: body.Metadata.Labels, Owner: in.Account.Meta.Owner}
	var opts LimitLink
	if s.Link != nil {
		opts = *s.Link
	}
	var limit Row
	var link func(*policy.Spec)
	if s.Budget != nil {
		b := &budget.Budget{Meta: m, Spec: *s.Budget}
		name(in.Names, "budget", &b.Meta)
		limit = Row{Kind: "budget", Pointer: "/spec/budget", Meta: &b.Meta, Value: b}
		link = func(p *policy.Spec) {
			p.Budgets = append(p.Budgets, policy.BudgetLink{BudgetID: b.Meta.ID, Shared: opts.Shared})
		}
	} else {
		rl := &ratelimit.RateLimit{Meta: m, Spec: *s.RateLimit}
		name(in.Names, "rate-limit", &rl.Meta)
		limit = Row{Kind: "rate-limit", Pointer: "/spec/rateLimit", Meta: &rl.Meta, Value: rl}
		link = func(p *policy.Spec) {
			p.RateLimits = append(p.RateLimits, policy.RateLimitLink{RateLimitID: rl.Meta.ID, Models: slices.Clone(opts.Models), Shared: opts.Shared})
		}
	}
	c, err := ChangePolicy(in, link, limit.Pointer)
	if err != nil {
		return LimitsChange{}, err
	}
	c.Rows = append([]Row{limit}, c.Rows...)
	return LimitsChange{PolicyChange: c, Limit: limit}, nil
}

// CheckLimits refuses a body whose target is malformed or that names not exactly one limit; it needs no read, so a caller runs it before reading what the body names. ExpandLimits runs it too.
func CheckLimits(body Limits) error {
	if err := CheckTarget(body.Spec.Target); err != nil {
		return err
	}
	s := body.Spec
	if (s.Budget == nil) == (s.RateLimit == nil) {
		return &BodyError{Pointer: "/spec", Message: "exactly one of spec.budget and spec.rateLimit is required"}
	}
	if s.Budget != nil && s.Link != nil && len(s.Link.Models) > 0 {
		return &BodyError{Pointer: "/spec/link/models", Message: "spec.link.models applies to a rate limit only: a budget link counts spend on every model"}
	}
	return nil
}

// name keeps the name m was given, held from the rows minted after it, or mints one of kind from its display name.
func name(names *Namer, kind string, m *meta.Metadata) {
	if m.Name != "" {
		names.Hold(kind, m.Name)
		return
	}
	m.Name = names.Mint(kind, m.DisplayName, kind)
}
