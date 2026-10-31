// Package policy is the domain layer for the Policy entity — the named
// group that bundles Models (m:n), ProviderKeys (m:n), and an optional
// single RateLimit (m:1, the one rule set that applies to traffic through
// this Policy).
//
// Wire format: Policy YAML/JSON carries entity *names*; the admin and seed
// boundaries resolve names to ids.
//
// Go domain shape: Policy.Spec carries id arrays / scalar id. Callers read
// the lists directly without dealing with joins.
//
// PG storage: arrays land in junction tables (policy_models,
// policy_provider_keys) with proper FKs; the single RateLimit ref is a
// column on the policies table. Policy's store.go fans the writes out
// across the junctions inside a transaction and rebuilds the arrays on
// read. (Migration + sqlc queries land as a follow-up; current store.go
// keeps Spec in JSONB until then, see TODO in store.go.)
//
// Reverse direction: Key carries a single Spec.PolicyID (m:1).
// Provider names its default via Spec.DefaultPolicyID. Models and
// ProviderKeys carry no policy reference — they are discovered via the
// junctions / via Policy.Spec.
package policy

import (
	"fmt"
	"slices"
	"strings"

	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/modelref"
	"github.com/wyolet/relay/app/ratelimit"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// Policy is the routing/auth bundle. Composition layer enforces that every
// id in the lists below resolves and (for ProviderKeys) shares the Provider
// of the Models in the same Policy.
type Policy struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec carries the membership lists, the single rate-limit, and the
// strategy knobs.
type Spec struct {
	// Models is the catalog grant — a list of ref strings parsed by
	// app/modelref. See its package doc for the grammar. Each entry can
	// match a single binding ("anthropic/claude-opus-4-7@bedrock"), all
	// hosts for a model ("anthropic/claude-opus-4-7" or trailing @*),
	// or every model under a provider ("anthropic" or "anthropic/*").
	//
	// Patterns expand against the live catalog at snapshot build time,
	// so a wildcard automatically includes models added later.
	//
	// **Empty Models AND empty ModelIDs is treated as an implicit
	// wildcard**: every model reachable through the policy's HostKeys is
	// allowed. The hostkey-coverage check at routing time is the real
	// authorization gate in that case; Models/ModelIDs, when set, narrow
	// that gate. Deprecated models are hidden from the implicit wildcard
	// unless Spec.IncludeDeprecated.
	Models []string `json:"models,omitempty" yaml:"models,omitempty" validate:"omitempty,dive,min=1"`

	// ModelIDs is the legacy literal-ID grant — exact Model UUIDs, no
	// wildcards. Coexists with Models during the transition; the
	// snapshot expands both into the same bindingsByPolicy index.
	// New grants should prefer Models. See Models above for the
	// empty-list = implicit-wildcard rule.
	ModelIDs []string `json:"modelIds,omitempty" yaml:"modelIds,omitempty" validate:"omitempty,dive,uuid"`

	// HostKeyIDs is the set of HostKeys this Policy can draw from
	// (m:n with ProviderKey). Order is significant for KeySelectionPrioritized.
	HostKeyIDs []string `json:"hostKeyIds,omitempty" yaml:"hostKeyIds,omitempty" validate:"omitempty,dive,uuid"`

	// RateLimitID is a single RateLimit applied uniformly to every model
	// the policy grants. Mutually exclusive with RLBindings — pick one or
	// the other (or neither for an uncapped policy). The flat form is the
	// common case for user-authored policies that don't differentiate
	// caps per model.
	RateLimitID string `json:"rateLimitId,omitempty" yaml:"rateLimitId,omitempty" validate:"omitempty,uuid"`

	// RLBindings declares per-model rate-limit mappings. Use this when
	// caps vary by the model being called (the typical host-tier case
	// where, e.g., gpt-4o has different TPM than gpt-4o-mini at the same
	// upstream tier). At request time the first binding whose Models
	// matches the requested model contributes its RateLimit's rules; a
	// model not covered by any binding is uncapped at this policy.
	//
	// Each binding must declare at least one entry in Models. Mutually
	// exclusive with RateLimitID — Validate rejects setting both.
	RLBindings []RLBinding `json:"rlBindings,omitempty" yaml:"rlBindings,omitempty" validate:"omitempty,dive"`

	// KeySelection is the algorithm used to pick a ProviderKey from the
	// healthy pool. Defaults to prioritized. The enum lives in app/keypool
	// (the consumer); policy re-exports the constants via type aliases
	// for backward source-compatibility.
	KeySelection keypool.KeySelection `json:"keySelection,omitempty" yaml:"keySelection,omitempty" validate:"omitempty,oneof=prioritized round-robin least-recently-used"`

	// IncludeDeprecated controls whether wildcard Models entries expand
	// to models whose Spec.Deprecation.Status is "deprecated" or
	// "sunset". Default false (deprecated models excluded). Literal
	// grants in Models always resolve regardless — if you explicitly
	// name a deprecated model by slug, you mean to grant it.
	IncludeDeprecated bool `json:"includeDeprecated,omitempty" yaml:"includeDeprecated,omitempty"`

	// Enabled defaults to true when nil.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`

	// PayloadLoggingEnabled decides whether the request/response bodies of
	// every request this policy governs are captured, overriding the
	// credential's own flag either way (see CapturesPayload). Off by default.
	PayloadLoggingEnabled bool `json:"payloadLoggingEnabled,omitempty" yaml:"payloadLoggingEnabled,omitempty"`

	// PoolIDs are the pools this policy draws keys from, tried in order, so a later pool is the failover for an earlier one. Host-owned policies list none.
	PoolIDs []string `json:"poolIds,omitempty" yaml:"poolIds,omitempty" validate:"omitempty,dive,uuid"`

	// CredentialIDs are host keys linked directly, each one a pool of one. Replaces HostKeyIDs; a policy sets one or the other. Host-owned policies list none.
	CredentialIDs []string `json:"credentialIds,omitempty" yaml:"credentialIds,omitempty" validate:"omitempty,dive,uuid"`

	// RateLimits are the rate limits a request under this policy must fit. Replaces RateLimitID and RLBindings; a policy sets one form.
	RateLimits []RateLimitLink `json:"rateLimits,omitempty" yaml:"rateLimits,omitempty" validate:"omitempty,dive"`

	// BudgetIDs are the budgets a request under this policy must fit.
	BudgetIDs []string `json:"budgetIds,omitempty" yaml:"budgetIds,omitempty" validate:"omitempty,dive,uuid"`
}

// RateLimitLink applies one RateLimit to a policy's requests: to every model when Models is empty, otherwise only to requests for a model matching one of Models (app/modelref grammar). One rate limit may be linked more than once with different Models.
type RateLimitLink struct {
	RateLimitID string   `json:"rateLimitId"      yaml:"rateLimitId"      validate:"required,uuid"`
	Models      []string `json:"models,omitempty" yaml:"models,omitempty" validate:"omitempty,dive,min=1"`
}

// RLBinding maps a set of Models to one RateLimit. The Models list uses
// the modelref DSL (same grammar as Policy.Spec.Models): a single binding
// can name explicit models, a provider/host wildcard, etc. Resolution
// scans bindings in declared order and the first match wins; authors
// should keep model sets disjoint within a policy.
type RLBinding struct {
	Models      []string `json:"models"      yaml:"models"      validate:"required,min=1,dive,min=1"`
	RateLimitID string   `json:"rateLimitId" yaml:"rateLimitId" validate:"required,uuid"`
}

// KeySelection is an alias for keypool.KeySelection so existing callers
// can keep writing policy.KeySelection without an import change. The
// canonical definition lives in app/keypool.
type KeySelection = keypool.KeySelection

// Re-exported constants for source compatibility.
const (
	KeySelectionPrioritized       = keypool.KeySelectionPrioritized
	KeySelectionRoundRobin        = keypool.KeySelectionRoundRobin
	KeySelectionLeastRecentlyUsed = keypool.KeySelectionLeastRecentlyUsed
)

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

// EffectiveKeySelection returns KeySelection or the prioritized default.
func (p *Policy) EffectiveKeySelection() keypool.KeySelection {
	if p.Spec.KeySelection == "" {
		return keypool.KeySelectionPrioritized
	}
	return p.Spec.KeySelection
}

// SelectRateLimitID returns the id of the RateLimit that applies to one
// request triple, or "" when the policy doesn't cap this request, plus
// whether the match came from a per-model RLBinding (which buckets per
// model rather than once for the whole policy).
//
// Resolution: when Spec.RLBindings is non-empty, the first binding whose
// Models matches the (provider, model, host) triple wins. Otherwise the
// flat Spec.RateLimitID is used. A request that matches no binding and
// has no flat fallback is uncapped at this policy.
//
// Pure query — no snapshot lookup, no I/O. Caller resolves the id to a
// *ratelimit.RateLimit via the snapshot.
func (p *Policy) SelectRateLimitID(providerSlug, modelSlug, hostSlug string) (id string, perModel bool) {
	if p == nil {
		return "", false
	}
	for _, b := range p.Spec.RLBindings {
		if modelref.MatchAny(b.Models, providerSlug, modelSlug, hostSlug) {
			return b.RateLimitID, true
		}
	}
	return p.Spec.RateLimitID, false
}

// ResolveRules converts the given RateLimit into the limiter's Rule
// shape, scoped by this policy's id, the RateLimit, and — for a
// per-model RLBinding — modelID. Returns nil when rl is nil, disabled, or
// has no Rules. This is the runtime equivalent of the old
// ratelimit.Resolve(pol, rl) function — moved here so app/ratelimit
// stays free of policy imports.
func (p *Policy) ResolveRules(rl *ratelimit.RateLimit, modelID string) []pkgratelimit.Rule {
	if p == nil || rl == nil {
		return nil
	}
	return ratelimit.ResolveWithScope("policy", ruleSubject(p.Meta.ID, rl.Meta.ID, modelID), rl)
}

// Validate runs intra-row rules via the shared meta.Validator and enforces:
//   - Owner.Kind is user or system.
//   - no id list repeats an entry.
//   - host-owned policies link no pools or credentials.
//   - rateLimits is not mixed with rateLimitId/rlBindings, nor credentialIds with hostKeyIds.
//
// Cross-entity checks (every id resolves; ProviderKeys + Models share a
// Provider; RateLimitID resolves; auth-required Providers have at least
// one resolvable ProviderKey) live in the composition layer.
func (p *Policy) Validate() error {
	if err := meta.Validator.Struct(p); err != nil {
		return err
	}
	switch p.Meta.Owner.Kind {
	case meta.OwnerUser, meta.OwnerSystem:
	case meta.OwnerHost:
		// Host-owned policies are tier definitions published by a Host as
		// part of its menu. They carry rules and (optionally) model lists
		// but never inbound grants — HostKeyIDs is meaningless for an
		// upstream tier.
		if p.Meta.Owner.ID == "" {
			return fmt.Errorf("policy %q: owner.id is required for host-owned policy", p.Meta.Name)
		}
		if len(p.Spec.HostKeyIDs) > 0 {
			return fmt.Errorf("policy %q: host-owned policies must not list hostKeyIds", p.Meta.Name)
		}
		if len(p.Spec.PoolIDs) > 0 || len(p.Spec.CredentialIDs) > 0 {
			return fmt.Errorf("policy %q: host-owned policies must not list poolIds or credentialIds", p.Meta.Name)
		}
	case meta.OwnerProject:
	default:
		return fmt.Errorf("policy %q: owner.kind required (user|system|host|project)", p.Meta.Name)
	}
	// RateLimit shape: flat singular OR per-model bindings, never both.
	if p.Spec.RateLimitID != "" && len(p.Spec.RLBindings) > 0 {
		return fmt.Errorf("policy %q: rateLimitId and rlBindings are mutually exclusive", p.Meta.Name)
	}
	if len(p.Spec.RateLimits) > 0 && (p.Spec.RateLimitID != "" || len(p.Spec.RLBindings) > 0) {
		return fmt.Errorf("policy %q: rateLimits replaces rateLimitId and rlBindings; set one form", p.Meta.Name)
	}
	if len(p.Spec.CredentialIDs) > 0 && len(p.Spec.HostKeyIDs) > 0 {
		return fmt.Errorf("policy %q: credentialIds replaces hostKeyIds; set one", p.Meta.Name)
	}
	for _, f := range []struct {
		name string
		ids  []string
	}{
		{"modelIds", p.Spec.ModelIDs},
		{"hostKeyIds", p.Spec.HostKeyIDs},
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
