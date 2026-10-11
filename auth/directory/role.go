package directory

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/wyolet/relay/auth/rbac"
)

// Role is a named rule set. It carries no scope; the binding that names it supplies one.
type Role struct {
	Meta Metadata
	Spec RoleSpec
}

// RoleSpec holds the rules and the enabled flag.
type RoleSpec struct {
	Rules []rbac.Rule
	// Enabled nil means enabled.
	Enabled *bool
	// Attributes are product fields stored next to the spec, keyed by field name; the module round-trips them untouched.
	Attributes map[string]json.RawMessage
}

// IsEnabled reports whether the role is enabled.
func (r *Role) IsEnabled() bool { return enabled(r.Spec.Enabled) }

// RBAC is the role as the engine reads it.
func (r *Role) RBAC() rbac.Role { return rbac.Role{ID: r.Meta.ID, Rules: r.Spec.Rules} }

// validate checks the rules against the product's vocabulary; a nil vocabulary accepts any non-empty entry.
func (r *Role) validate(kinds, verbs []string) error {
	if err := validateMeta("role", &r.Meta, OwnerSystem, OwnerUser); err != nil {
		return err
	}
	if len(r.Spec.Rules) == 0 {
		return fmt.Errorf("%w: role %q: at least one rule is required", ErrInvalid, r.Meta.Name)
	}
	for i, rule := range r.Spec.Rules {
		if err := checkTerms(rule.Kinds, kinds); err != nil {
			return fmt.Errorf("%w: role %q: rule %d kinds: %v", ErrInvalid, r.Meta.Name, i, err)
		}
		if err := checkTerms(rule.Verbs, verbs); err != nil {
			return fmt.Errorf("%w: role %q: rule %d verbs: %v", ErrInvalid, r.Meta.Name, i, err)
		}
	}
	return nil
}

// checkTerms makes a typo in a rule an error instead of a rule that silently matches nothing.
func checkTerms(terms, vocabulary []string) error {
	if len(terms) == 0 {
		return fmt.Errorf("at least one is required")
	}
	for _, t := range terms {
		if t == "" {
			return fmt.Errorf("empty entry")
		}
		if t != rbac.Wildcard && vocabulary != nil && !slices.Contains(vocabulary, t) {
			return fmt.Errorf("%q is not a registered name", t)
		}
	}
	return nil
}
