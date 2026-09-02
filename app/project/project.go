// Package project is the domain layer for the Project entity — the unit
// inside a Team that owns Policies, RelayKeys, and user-created HostKeys.
// A Project's owner is always its Team (owner.kind=team, owner.id=teamId);
// rows owned by a Project carry owner.kind=project.
//
// Authorization (who in the team may touch a project's rows) lives in
// app/authz; this package validates shape only.
package project

import (
	"fmt"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/team"
)

// Project is a Team-owned grouping of request-authoring resources.
type Project struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec carries the parent Team, the enabled flag, and the spend cap.
type Spec struct {
	// TeamID is the owning Team. Required; mirrors Meta.Owner.ID.
	TeamID string `json:"teamId" yaml:"teamId" validate:"required,uuid"`

	// Enabled defaults to true when nil.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`

	// Budget caps spend for this project alone.
	Budget *team.Budget `json:"budget,omitempty" yaml:"budget,omitempty" validate:"omitempty"`
}

// IsEnabled returns true when Enabled is unset or explicitly true.
func (p *Project) IsEnabled() bool { return p.Spec.Enabled == nil || *p.Spec.Enabled }

// StampOwner sets Meta.Owner to the project's Team. Called by every
// write path (translate, CRUD guard) so Owner never drifts from TeamID.
func (p *Project) StampOwner() {
	p.Meta.Owner = meta.Owner{Kind: meta.OwnerTeam, ID: p.Spec.TeamID}
}

// Validate runs intra-row rules via the shared meta.Validator and enforces:
//   - Owner.Kind is team; Owner.ID, when set, equals Spec.TeamID.
func (p *Project) Validate() error {
	if err := meta.Validator.Struct(p); err != nil {
		return err
	}
	if p.Meta.Owner.Kind != meta.OwnerTeam {
		return fmt.Errorf("project %q: owner.kind must be team, got %q", p.Meta.Name, p.Meta.Owner.Kind)
	}
	if p.Meta.Owner.ID != "" && p.Meta.Owner.ID != p.Spec.TeamID {
		return fmt.Errorf("project %q: owner.id %q does not match spec.teamId %q", p.Meta.Name, p.Meta.Owner.ID, p.Spec.TeamID)
	}
	return nil
}
