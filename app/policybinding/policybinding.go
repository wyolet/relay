// Package policybinding is the domain layer for the PolicyBinding entity: it applies one Policy to a set of accounts inside its owner's scope (the system, a team, a project or a user). Accounts are the only subjects: a team-wide cap binds the team's account, a member's the member account. An account is held by at most one binding.
//
// Authorization and the cross-row scope rules are app/authz's and app/refcheck's concern; this package validates shape only.
package policybinding

import (
	"fmt"

	"github.com/wyolet/relay/app/meta"
)

// PolicyBinding attaches Spec.PolicyID to Spec.AccountIDs inside the scope Meta.Owner names.
type PolicyBinding struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec carries the policy, the accounts, and the enabled flag.
type Spec struct {
	PolicyID   string   `json:"policyId"   yaml:"policyId"   validate:"required,uuid" doc:"Id of the policy the accounts are bound to."`
	AccountIDs []string `json:"accountIds" yaml:"accountIds" validate:"required,min=1,dive,uuid" doc:"Ids of the accounts the policy governs, each inside the binding owner's scope. An account is held by at most one binding; a second answers 409 duplicate_binding."`
	Enabled    *bool    `json:"enabled,omitempty" yaml:"enabled,omitempty" doc:"False stops the binding from governing its accounts. Omitted means true."`
}

// IsEnabled returns true when Enabled is unset or explicitly true.
func (b *PolicyBinding) IsEnabled() bool { return b.Spec.Enabled == nil || *b.Spec.Enabled }

// ScopeKey names the owner scope ("team:<id>", "project:<id>", "user:<id>"), "" for the system.
func (b *PolicyBinding) ScopeKey() string {
	if b.Meta.Owner.Kind == meta.OwnerSystem {
		return ""
	}
	return string(b.Meta.Owner.Kind) + ":" + b.Meta.Owner.ID
}

// Validate runs intra-row rules via the shared meta.Validator and enforces:
//   - Owner.Kind is system (no id), or team, project or user with an id.
//   - no account is listed twice.
func (b *PolicyBinding) Validate() error {
	if err := meta.Validator.Struct(b); err != nil {
		return err
	}
	switch o := b.Meta.Owner; o.Kind {
	case meta.OwnerSystem:
		if o.ID != "" {
			return fmt.Errorf("policybinding %q: a system owner has no id, got %q", b.Meta.Name, o.ID)
		}
	case meta.OwnerTeam, meta.OwnerProject, meta.OwnerUser:
		if meta.Validator.Var(o.ID, "required,uuid") != nil {
			return fmt.Errorf("policybinding %q: a %s owner needs its id, got %q", b.Meta.Name, o.Kind, o.ID)
		}
	default:
		return fmt.Errorf("policybinding %q: owner.kind must be system, team, project or user, got %q", b.Meta.Name, o.Kind)
	}
	seen := make(map[string]struct{}, len(b.Spec.AccountIDs))
	for i, id := range b.Spec.AccountIDs {
		if _, dup := seen[id]; dup {
			return fmt.Errorf("policybinding %q: accountIds[%d] %s is listed twice", b.Meta.Name, i, id)
		}
		seen[id] = struct{}{}
	}
	return nil
}
