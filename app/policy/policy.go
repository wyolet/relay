// Package policy is the domain layer for the Policy entity: the gate linking
// a subject to the resources it may use. A policy holds nothing of its own,
// only references: the models it allows, the pools and credentials it draws
// keys from, and the rate limits and budgets every request under it must fit.
// One kind serves every rung of the ladder; a host-owned policy is a tier
// preset and links no pools or credentials.
//
// Wire format: Policy YAML/JSON carries entity *names*; the admin and seed
// boundaries resolve names to ids. PG storage: Spec.Models sits in a column,
// the links in junction tables (see store.go).
//
// Out of scope: which subjects a policy governs (app/policybinding), and
// resolving the ladder per request (app/plan).
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

// Spec carries the model grant, the links, and the flags.
type Spec struct {
	// Models is the catalog grant — a list of ref strings parsed by
	// app/modelref. See its package doc for the grammar. Each entry can
	// match a single binding ("anthropic/claude-opus-4-7@bedrock"), all
	// hosts for a model ("anthropic/claude-opus-4-7"), or every model
	// under a provider ("anthropic" or "anthropic/*").
	//
	// Patterns expand against the live catalog at plan build time, so a
	// wildcard automatically includes models added later. Empty does not
	// constrain this rung: the parent's grant applies. Deprecated models
	// are hidden from wildcards unless Spec.IncludeDeprecated.
	Models []string `json:"models,omitempty" yaml:"models,omitempty" validate:"omitempty,dive,min=1" doc:"Model references this policy allows, in the model-reference grammar: provider/model@host for one binding, provider/model for every host serving it, provider or provider/* for every model of a provider. Wildcards expand against the live catalog. Empty does not constrain this rung: the request is held to the other rungs' lists."`

	// IncludeDeprecated controls whether wildcard Models entries expand
	// to models whose Spec.Deprecation.Status is "deprecated" or
	// "sunset". Default false (deprecated models excluded). Literal
	// grants in Models always resolve regardless — if you explicitly
	// name a deprecated model by slug, you mean to grant it.
	IncludeDeprecated bool `json:"includeDeprecated,omitempty" yaml:"includeDeprecated,omitempty" doc:"Let wildcard entries in models match deprecated and sunset models. Models named literally always match."`

	// Enabled defaults to true when nil.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty" doc:"False refuses every request this policy governs with policy_disabled. Omitted means true."`

	// PayloadLoggingEnabled decides whether the request/response bodies of
	// every request this policy governs are captured, overriding the
	// credential's own flag either way (see CapturesPayload). Off by default.
	PayloadLoggingEnabled bool `json:"payloadLoggingEnabled,omitempty" yaml:"payloadLoggingEnabled,omitempty" doc:"Capture request and response bodies of every request this policy governs, overriding the key's own flag."`

	// PoolIDs are the pools this policy draws keys from, tried in order, so a later pool is the failover for an earlier one. Host-owned policies list none.
	PoolIDs []string `json:"poolIds,omitempty" yaml:"poolIds,omitempty" validate:"omitempty,dive,uuid" doc:"Ids of the pools requests draw host keys from, tried in this order: a later pool is the failover for an earlier one. Only on policies bound to a team, project, user or service account."`

	// CredentialIDs are host keys linked directly, each one a pool of one. Host-owned policies list none.
	CredentialIDs []string `json:"credentialIds,omitempty" yaml:"credentialIds,omitempty" validate:"omitempty,dive,uuid" doc:"Ids of host keys linked directly, each one a pool of one, tried after poolIds. Only on policies bound to a team, project, user or service account."`

	// RateLimits are the rate limits a request under this policy must fit.
	RateLimits []RateLimitLink `json:"rateLimits,omitempty" yaml:"rateLimits,omitempty" validate:"omitempty,dive" doc:"Rate limits every request under this policy must fit."`

	// BudgetIDs are the budgets a request under this policy must fit.
	BudgetIDs []string `json:"budgetIds,omitempty" yaml:"budgetIds,omitempty" validate:"omitempty,dive,uuid" doc:"Ids of the budgets every request under this policy must fit. Unpriced models are refused under a budget."`
}

// RateLimitLink applies one RateLimit to a policy's requests: to every model when Models is empty, otherwise only to requests for a model matching one of Models (app/modelref grammar). One rate limit may be linked more than once with different Models.
type RateLimitLink struct {
	RateLimitID string   `json:"rateLimitId"      yaml:"rateLimitId"      validate:"required,uuid"    doc:"Id of the linked rate limit."`
	Models      []string `json:"models,omitempty" yaml:"models,omitempty" validate:"omitempty,dive,min=1" doc:"Model references the limit applies to; empty applies it to every request. Only policies bound to a host, account or host key may scope a limit to models."`
}

// IsEnabled returns true when Enabled is unset or explicitly true.
func (p *Policy) IsEnabled() bool { return p.Spec.Enabled == nil || *p.Spec.Enabled }

// CapturesPayload reports whether a request's bodies are captured. The
// governing policy decides, in both directions; credentialOptIn counts only
// when no policy governs. Nil governing means none does.
func CapturesPayload(governing *Policy, credentialOptIn bool) bool {
	if governing != nil {
		return governing.Spec.PayloadLoggingEnabled
	}
	return credentialOptIn
}

// Validate runs intra-row rules via the shared meta.Validator and enforces:
//   - Owner.Kind is system, host, team, project or user.
//   - no id list repeats an entry.
//   - host-owned policies link no pools or credentials.
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
		if len(p.Spec.PoolIDs) > 0 || len(p.Spec.CredentialIDs) > 0 {
			return fmt.Errorf("policy %q: host-owned policies must not list poolIds or credentialIds", p.Meta.Name)
		}
	default:
		return fmt.Errorf("policy %q: owner.kind required (user|system|host|team|project)", p.Meta.Name)
	}
	for _, f := range []struct {
		name string
		ids  []string
	}{
		{"poolIds", p.Spec.PoolIDs},
		{"credentialIds", p.Spec.CredentialIDs},
		{"budgetIds", p.Spec.BudgetIDs},
	} {
		if err := uniqueIDs(f.name, p.Meta.Name, f.ids); err != nil {
			return err
		}
	}
	if err := validateModelRefs(p.Meta.Name, p.Spec.Models); err != nil {
		return err
	}
	return validateRateLimitLinks(p.Meta.Name, p.Spec.RateLimits)
}

// validateRateLimitLinks checks each link's model refs and refuses two links of one rate limit over the same models, which would only count every request twice.
func validateRateLimitLinks(policyName string, links []RateLimitLink) error {
	seen := make(map[string]int, len(links))
	for i, l := range links {
		if err := validateModelRefs(policyName, l.Models); err != nil {
			return fmt.Errorf("rateLimits[%d]: %w", i, err)
		}
		models := slices.Clone(l.Models)
		slices.Sort(models)
		key := l.RateLimitID + "\x00" + strings.Join(models, "\x00")
		if j, dup := seen[key]; dup {
			return fmt.Errorf("policy %q: rateLimits[%d] and rateLimits[%d] link rate limit %q over the same models", policyName, j, i, l.RateLimitID)
		}
		seen[key] = i
	}
	return nil
}

// validateModelRefs runs the modelref parser over every entry and
// rejects duplicates. Parser errors carry their own message; the policy
// name is prepended for context.
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
		if _, err := modelref.Parse(raw); err != nil {
			return fmt.Errorf("policy %q: %w", policyName, err)
		}
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
