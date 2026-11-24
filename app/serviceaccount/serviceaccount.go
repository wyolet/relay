// Package serviceaccount is the domain layer for the ServiceAccount
// entity — a non-human principal that holds Keys. A ServiceAccount is owned
// by a Project or, with no project in between, by a Team; the owner is
// fixed at create, and spec.projectId or spec.teamId mirrors it.
//
// Authorization is app/authz's concern; this package validates shape only.
package serviceaccount

import (
	"fmt"

	"github.com/wyolet/relay/pkg/meta"
)

// ServiceAccount is a machine principal owned by a Project or a Team.
type ServiceAccount struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec carries the owner's id and the enabled flag. A service account
// holds no policy: a PolicyBinding binds it.
type Spec struct {
	// ProjectID mirrors Meta.Owner.ID when the owner is a Project. A row whose owner names no kind is owned by this project.
	ProjectID string `json:"projectId,omitempty" yaml:"projectId,omitempty" validate:"omitempty,uuid" doc:"Id of the owning project when metadata.owner is a project. A create that names only spec.projectId is owned by that project."`

	// TeamID mirrors Meta.Owner.ID when the owner is a Team.
	TeamID string `json:"teamId,omitempty" yaml:"teamId,omitempty" validate:"omitempty,uuid" readOnly:"true" doc:"Id of the owning team when metadata.owner is a team. Mirrors the owner; ignored on write."`

	// Enabled defaults to true when nil.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty" doc:"False refuses inference with the service account's keys (403 subject_disabled). Omitted means true."`
}

// IsEnabled returns true when Enabled is unset or explicitly true.
func (s *ServiceAccount) IsEnabled() bool { return s.Spec.Enabled == nil || *s.Spec.Enabled }

// StampOwner fills the spec field that mirrors Meta.Owner and clears the
// other. An owner without a kind becomes the project spec.projectId names,
// and a project owner without an id takes it from spec.projectId, so a body
// that names only spec.projectId is owned by that project. A project owner
// that spec.projectId contradicts is left for Validate to refuse. Called by
// every write path (translate, CRUD guard, store).
func (s *ServiceAccount) StampOwner() {
	o := &s.Meta.Owner
	if o.Kind == "" {
		*o = meta.Owner{Kind: meta.OwnerProject}
	}
	switch o.Kind {
	case meta.OwnerProject:
		if o.ID == "" {
			o.ID = s.Spec.ProjectID
		}
		if s.Spec.ProjectID == "" {
			s.Spec.ProjectID = o.ID
		}
		s.Spec.TeamID = ""
	case meta.OwnerTeam:
		s.Spec.TeamID = o.ID
	}
}

// Validate runs intra-row rules via the shared meta.Validator and enforces:
//   - Owner.Kind is project or team.
//   - A project owner's id, when set, equals Spec.ProjectID when that is set, and one of them is set.
//   - A team owner has an id and Spec.ProjectID is empty.
//
// Spec.TeamID is not checked: it is read-only, and StampOwner sets it.
func (s *ServiceAccount) Validate() error {
	if err := meta.Validator.Struct(s); err != nil {
		return err
	}
	o := s.Meta.Owner
	switch o.Kind {
	case meta.OwnerProject:
		if o.ID != "" && s.Spec.ProjectID != "" && o.ID != s.Spec.ProjectID {
			return fmt.Errorf("serviceaccount %q: owner.id %q does not match spec.projectId %q", s.Meta.Name, o.ID, s.Spec.ProjectID)
		}
		if o.ID == "" && s.Spec.ProjectID == "" {
			return fmt.Errorf("serviceaccount %q: a project owner needs metadata.owner.id or spec.projectId", s.Meta.Name)
		}
	case meta.OwnerTeam:
		if o.ID == "" {
			return fmt.Errorf("serviceaccount %q: a team owner needs metadata.owner.id", s.Meta.Name)
		}
		if s.Spec.ProjectID != "" {
			return fmt.Errorf("serviceaccount %q: owner is team %q but spec.projectId names project %q; a service account has one owner", s.Meta.Name, o.ID, s.Spec.ProjectID)
		}
	default:
		return fmt.Errorf("serviceaccount %q: owner.kind must be project or team, got %q", s.Meta.Name, o.Kind)
	}
	return nil
}
