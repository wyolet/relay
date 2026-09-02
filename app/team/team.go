// Package team is the domain layer for the Team entity — the outer
// tenancy scope that owns Projects. A Team has no member list:
// membership is expressed as a role binding at team scope, so there is
// exactly one place that grants access.
//
// Authorization is app/authz's concern; this package validates shape only.
package team

import (
	"fmt"

	"github.com/wyolet/relay/app/meta"
)

// Team is the outer tenancy scope. Owner is the user who created it
// (user kind) or system for seeded rows.
type Team struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec carries the enabled flag and the team-wide spend cap.
type Spec struct {
	// Enabled defaults to true when nil.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`

	// Budget caps spend across every Project in the team.
	Budget *Budget `json:"budget,omitempty" yaml:"budget,omitempty" validate:"omitempty"`
}

// IsEnabled returns true when Enabled is unset or explicitly true.
func (t *Team) IsEnabled() bool { return t.Spec.Enabled == nil || *t.Spec.Enabled }

// Validate runs intra-row rules via the shared meta.Validator and enforces:
//   - Owner.Kind is user or system.
func (t *Team) Validate() error {
	if err := meta.Validator.Struct(t); err != nil {
		return err
	}
	switch t.Meta.Owner.Kind {
	case meta.OwnerUser, meta.OwnerSystem:
	default:
		return fmt.Errorf("team %q: owner.kind must be user or system, got %q", t.Meta.Name, t.Meta.Owner.Kind)
	}
	return nil
}
