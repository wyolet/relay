// Package serviceaccount is the domain layer for the ServiceAccount
// entity — a non-human principal that lives inside a Project and holds
// Keys. A ServiceAccount's owner is always its Project
// (owner.kind=project, owner.id=projectId).
//
// Authorization is app/authz's concern; this package validates shape only.
package serviceaccount

import (
	"fmt"

	"github.com/wyolet/relay/app/meta"
)

// ServiceAccount is a Project-owned machine principal.
type ServiceAccount struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec carries the parent Project and the enabled flag. A service account
// holds no policy: a PolicyBinding binds it.
type Spec struct {
	// ProjectID is the owning Project. Required; mirrors Meta.Owner.ID.
	ProjectID string `json:"projectId" yaml:"projectId" validate:"required,uuid" doc:"Id of the project the service account belongs to. The owner follows it."`

	// Enabled defaults to true when nil.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty" doc:"False refuses inference with the service account's keys (403 subject_disabled). Omitted means true."`
}

// IsEnabled returns true when Enabled is unset or explicitly true.
func (s *ServiceAccount) IsEnabled() bool { return s.Spec.Enabled == nil || *s.Spec.Enabled }

// StampOwner sets Meta.Owner to the account's Project. Called by every
// write path (translate, CRUD guard) so Owner never drifts from ProjectID.
func (s *ServiceAccount) StampOwner() {
	s.Meta.Owner = meta.Owner{Kind: meta.OwnerProject, ID: s.Spec.ProjectID}
}

// Validate runs intra-row rules via the shared meta.Validator and enforces:
//   - Owner.Kind is project; Owner.ID, when set, equals Spec.ProjectID.
func (s *ServiceAccount) Validate() error {
	if err := meta.Validator.Struct(s); err != nil {
		return err
	}
	if s.Meta.Owner.Kind != meta.OwnerProject {
		return fmt.Errorf("serviceaccount %q: owner.kind must be project, got %q", s.Meta.Name, s.Meta.Owner.Kind)
	}
	if s.Meta.Owner.ID != "" && s.Meta.Owner.ID != s.Spec.ProjectID {
		return fmt.Errorf("serviceaccount %q: owner.id %q does not match spec.projectId %q", s.Meta.Name, s.Meta.Owner.ID, s.Spec.ProjectID)
	}
	return nil
}
