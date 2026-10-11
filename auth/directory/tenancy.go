package directory

import (
	"encoding/json"
	"fmt"

	"github.com/wyolet/relay/auth/authz"
)

// Team is the outer tenancy scope; it owns projects. Membership is a role binding at the team's scope, so there is one place that grants access.
type Team struct {
	Meta Metadata
	Spec TeamSpec
}

// TeamSpec holds the enabled flag.
type TeamSpec struct {
	// Enabled nil means enabled.
	Enabled *bool
	// Attributes are product fields stored next to the spec, keyed by field name; the module round-trips them untouched.
	Attributes map[string]json.RawMessage
}

// IsEnabled reports whether the team is enabled.
func (t *Team) IsEnabled() bool { return enabled(t.Spec.Enabled) }

func (t *Team) validate() error { return validateMeta("team", &t.Meta, OwnerUser, OwnerSystem) }

// Project lives in one team and owns the product's rows that carry it as their owner. Its owner is always its team.
type Project struct {
	Meta Metadata
	Spec ProjectSpec
}

// ProjectSpec holds the parent team and the enabled flag.
type ProjectSpec struct {
	TeamID string
	// Enabled nil means enabled.
	Enabled *bool
	// Attributes are product fields stored next to the spec, keyed by field name; the module round-trips them untouched.
	Attributes map[string]json.RawMessage
}

// IsEnabled reports whether the project is enabled.
func (p *Project) IsEnabled() bool { return enabled(p.Spec.Enabled) }

// StampOwner sets the owner to the project's team, so it never drifts from TeamID.
func (p *Project) StampOwner() { p.Meta.Owner = authz.Owner{Kind: OwnerTeam, ID: p.Spec.TeamID} }

func (p *Project) validate() error {
	if p.Spec.TeamID == "" {
		return fmt.Errorf("%w: project %q: team id is required", ErrInvalid, p.Meta.Name)
	}
	return validateMeta("project", &p.Meta)
}

// ServiceAccount is a machine principal that lives in one project. Its owner is always its project.
type ServiceAccount struct {
	Meta Metadata
	Spec ServiceAccountSpec
}

// ServiceAccountSpec holds the parent project and the enabled flag.
type ServiceAccountSpec struct {
	ProjectID string
	// Enabled nil means enabled.
	Enabled *bool
	// Attributes are product fields stored next to the spec, keyed by field name; the module round-trips them untouched.
	Attributes map[string]json.RawMessage
}

// IsEnabled reports whether the service account is enabled.
func (s *ServiceAccount) IsEnabled() bool { return enabled(s.Spec.Enabled) }

// StampOwner sets the owner to the account's project, so it never drifts from ProjectID.
func (s *ServiceAccount) StampOwner() {
	s.Meta.Owner = authz.Owner{Kind: OwnerProject, ID: s.Spec.ProjectID}
}

func (s *ServiceAccount) validate() error {
	if s.Spec.ProjectID == "" {
		return fmt.Errorf("%w: service account %q: project id is required", ErrInvalid, s.Meta.Name)
	}
	return validateMeta("service account", &s.Meta)
}
