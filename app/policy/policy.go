// Package policy is the domain layer for the Policy entity: a reusable rule set a policy binding applies to accounts. A policy holds nothing of its own, only references: the models it allows, the pools it draws keys from, and the rate limits and budgets every request under it must fit, each counted once for the bound account (shared) or once per account under it. One kind serves every rung of the ladder; a host-owned policy is a tier preset and links no pools.
//
// Wire format: Policy YAML/JSON carries entity *names*; the admin and seed boundaries resolve names to ids. PG storage: Spec.Models sits in a column, the links in junction tables (see store.go).
//
// Out of scope: which accounts a policy governs (app/policybinding), and resolving the ladder per request (app/plan).
package policy

import (
	"fmt"
	"slices"
	"strings"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/modelref"
)

// Policy is the access gate. Composition enforces that every linked id
// resolves and that the author may use it.
type Policy struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec carries the model grant, the links, and the enabled flag.
type Spec struct {
	// Models is the catalog grant: "provider" for every model of a provider, "provider/model" for one model. Empty grants every model the policy's pools can serve. Hosts are the pools' business, so a ref naming one is refused.
	Models []string `json:"models" yaml:"models,omitempty" required:"false" validate:"omitempty,dive,min=1" doc:"Models this policy allows: provider for every model of a provider, provider/model for one model (provider/* is stored as provider). Empty ([]) allows every model the reached pools serve. A host (@host) is refused: the pools decide hosts. The request is held to every rung's list."`

	// Enabled defaults to true when nil.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty" doc:"False refuses every request this policy governs with policy_disabled. Omitted means true."`

	// PoolIDs are the pools this policy draws keys from, tried in order, so a later pool is the failover for an earlier one. Host-owned policies list none.
	PoolIDs []string `json:"poolIds,omitempty" yaml:"poolIds,omitempty" validate:"omitempty,dive,uuid" doc:"Ids of the pools requests draw host keys from, tried in this order: a later pool is the failover for an earlier one. Not on host-owned (tier) policies."`

	// RateLimits are the rate limits a request under this policy must fit.
	RateLimits []RateLimitLink `json:"rateLimits,omitempty" yaml:"rateLimits,omitempty" validate:"omitempty,dive" doc:"Rate limits every request under this policy must fit."`

	// Budgets are the budgets a request under this policy must fit.
	Budgets []BudgetLink `json:"budgets,omitempty" yaml:"budgets,omitempty" validate:"omitempty,dive" doc:"Budgets every request under this policy must fit. Unpriced models are refused under a budget."`
}

// RateLimitLink applies one RateLimit to a policy's requests: to every model when Models is empty, otherwise only to requests for a model matching one of Models. One rate limit may be linked more than once with different Models or counting.
type RateLimitLink struct {
	RateLimitID string   `json:"rateLimitId"      yaml:"rateLimitId"      validate:"required,uuid"    doc:"Id of the linked rate limit."`
	Models      []string `json:"models,omitempty" yaml:"models,omitempty" validate:"omitempty,dive,min=1" doc:"Models the limit applies to, in the policy's model grammar (provider or provider/model); empty applies it to every request. Each matched model counts on its own."`
	// Shared reads back explicit; nil on write means true.
	Shared *bool `json:"shared,omitempty" yaml:"shared,omitempty" doc:"True counts once for the bound account; false counts once per account under it (each lane its own bucket). Omitted on write means true; always returned."`
}

// BudgetLink applies one Budget to a policy's requests.
type BudgetLink struct {
	BudgetID string `json:"budgetId" yaml:"budgetId" validate:"required,uuid" doc:"Id of the linked budget."`
	// Shared reads back explicit; nil on write means true.
	Shared *bool `json:"shared,omitempty" yaml:"shared,omitempty" doc:"True spends one budget for the bound account; false gives each account under it a budget of its own. Omitted on write means true; always returned."`
}

// IsShared reports whether the link counts once for the bound account.
func (l *RateLimitLink) IsShared() bool { return l.Shared == nil || *l.Shared }

// IsShared reports whether the link spends one budget for the bound account.
func (l *BudgetLink) IsShared() bool { return l.Shared == nil || *l.Shared }

// IsEnabled returns true when Enabled is unset or explicitly true.
func (p *Policy) IsEnabled() bool { return p.Spec.Enabled == nil || *p.Spec.Enabled }

// Normalize gives the read form every boundary returns: models as a list (never null) and every link's shared flag explicit.
func (p *Policy) Normalize() {
	if p.Spec.Models == nil {
		p.Spec.Models = []string{}
	}
	for i := range p.Spec.RateLimits {
		l := &p.Spec.RateLimits[i]
		l.Shared = explicit(l.Shared)
	}
	for i := range p.Spec.Budgets {
		l := &p.Spec.Budgets[i]
		l.Shared = explicit(l.Shared)
	}
}

func explicit(b *bool) *bool {
	v := b == nil || *b
	return &v
}

// BudgetIDs returns the ids of the linked budgets, in link order.
func (p *Policy) BudgetIDs() []string {
	out := make([]string, len(p.Spec.Budgets))
	for i, b := range p.Spec.Budgets {
		out[i] = b.BudgetID
	}
	return out
}

// Validate runs intra-row rules via the shared meta.Validator and enforces:
//   - Owner.Kind is system, host, team, project or user.
//   - no pool or budget is linked twice.
//   - host-owned policies link no pools.
//   - every model ref names a provider or a provider's model, never a host.
//
// Cross-entity checks (every id resolves; the author may use it) live in
// app/refcheck.
func (p *Policy) Validate() error {
	if err := meta.Validator.Struct(p); err != nil {
		return err
	}
	switch p.Meta.Owner.Kind {
	case meta.OwnerUser, meta.OwnerSystem, meta.OwnerProject, meta.OwnerTeam:
	case meta.OwnerHost:
		// A host-owned policy is a tier preset: what the provider grants an
		// account. It caps; it never hands out keys.
		if p.Meta.Owner.ID == "" {
			return fmt.Errorf("policy %q: owner.id is required for host-owned policy", p.Meta.Name)
		}
		if len(p.Spec.PoolIDs) > 0 {
			return fmt.Errorf("policy %q: host-owned policies must not list poolIds", p.Meta.Name)
		}
	default:
		return fmt.Errorf("policy %q: owner.kind required (user|system|host|team|project)", p.Meta.Name)
	}
	if err := uniqueIDs("poolIds", p.Meta.Name, p.Spec.PoolIDs); err != nil {
		return err
	}
	if err := uniqueIDs("budgets", p.Meta.Name, p.BudgetIDs()); err != nil {
		return err
	}
	if err := validateModelRefs(p.Meta.Name, p.Spec.Models); err != nil {
		return err
	}
	return validateRateLimitLinks(p.Meta.Name, p.Spec.RateLimits)
}

// validateRateLimitLinks checks each link's model refs and refuses two links of one rate limit over the same models and counting, which would only count every request twice.
func validateRateLimitLinks(policyName string, links []RateLimitLink) error {
	seen := make(map[string]int, len(links))
	for i, l := range links {
		if err := validateModelRefs(policyName, l.Models); err != nil {
			return fmt.Errorf("rateLimits[%d]: %w", i, err)
		}
		models := slices.Clone(l.Models)
		slices.Sort(models)
		key := fmt.Sprintf("%s\x00%t\x00%s", l.RateLimitID, l.IsShared(), strings.Join(models, "\x00"))
		if j, dup := seen[key]; dup {
			return fmt.Errorf("policy %q: rateLimits[%d] and rateLimits[%d] link rate limit %q over the same models", policyName, j, i, l.RateLimitID)
		}
		seen[key] = i
	}
	return nil
}

// validateModelRefs parses every entry, rejects duplicates, and refuses a ref naming a host: pools decide hosts, so a policy names providers and models only.
func validateModelRefs(policyName string, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(refs))
	for _, raw := range refs {
		if _, dup := seen[raw]; dup {
			return fmt.Errorf("policy %q: duplicate models entry %q", policyName, raw)
		}
		seen[raw] = struct{}{}
		r, err := modelref.Parse(raw)
		if err != nil {
			return fmt.Errorf("policy %q: %w", policyName, err)
		}
		if err := grantable(r); err != nil {
			return fmt.Errorf("policy %q: models entry %q %w", policyName, raw, err)
		}
	}
	return nil
}

// grantable refuses a ref a policy cannot hold: one naming a host, or a model without its provider.
func grantable(r modelref.Ref) error {
	switch {
	case !r.HostWildcard:
		return fmt.Errorf("names a host; a policy names a provider or a provider's model, and its pools decide the hosts")
	case r.ProviderWildcard:
		return fmt.Errorf("names no provider; write provider or provider/model")
	}
	return nil
}

func uniqueIDs(field, owner string, ids []string) error {
	if len(ids) < 2 {
		return nil
	}
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			return fmt.Errorf("policy %q: duplicate %s entry %q", owner, field, id)
		}
		seen[id] = struct{}{}
	}
	return nil
}
