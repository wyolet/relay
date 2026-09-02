// Package policybinding is the domain layer for the PolicyBinding entity —
// it points a set of subjects inside one Project at one Policy. Several
// bindings may match a principal; the lowest priority wins, so a principal
// always resolves to exactly one Policy.
//
// Authorization is app/authz's concern; this package validates shape only.
package policybinding

import (
	"fmt"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/rolebinding"
)

// DefaultPriority is applied to a binding that declares none. Lower wins.
const DefaultPriority = 100

// PolicyBinding attaches Spec.PolicyID to Spec.Subjects inside a Project.
type PolicyBinding struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec carries the project, the policy, the tie-break priority, the
// subjects, and the enabled flag.
type Spec struct {
	ProjectID string                `json:"projectId" yaml:"projectId" validate:"required,uuid"`
	PolicyID  string                `json:"policyId"  yaml:"policyId"  validate:"required,uuid"`
	Priority  int                   `json:"priority,omitempty" yaml:"priority,omitempty" validate:"gte=0,lte=10000"`
	Subjects  []rolebinding.Subject `json:"subjects"  yaml:"subjects"  validate:"required,min=1,dive"`
	Enabled   *bool                 `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

// IsEnabled returns true when Enabled is unset or explicitly true.
func (b *PolicyBinding) IsEnabled() bool { return b.Spec.Enabled == nil || *b.Spec.Enabled }

// EffectivePriority reads an unset (zero) priority as the default, so a
// binding authored without one still orders against explicit values.
func (b *PolicyBinding) EffectivePriority() int {
	if b.Spec.Priority == 0 {
		return DefaultPriority
	}
	return b.Spec.Priority
}

// StampOwner sets Meta.Owner to the binding's Project. Called by every
// write path so Owner never drifts from ProjectID.
func (b *PolicyBinding) StampOwner() {
	b.Meta.Owner = meta.Owner{Kind: meta.OwnerProject, ID: b.Spec.ProjectID}
}

// Validate runs intra-row rules via the shared meta.Validator and enforces:
//   - Owner.Kind is project; Owner.ID, when set, equals Spec.ProjectID.
//   - the subject rules shared with RoleBinding.
func (b *PolicyBinding) Validate() error {
	if err := meta.Validator.Struct(b); err != nil {
		return err
	}
	if b.Meta.Owner.Kind != meta.OwnerProject {
		return fmt.Errorf("policybinding %q: owner.kind must be project, got %q", b.Meta.Name, b.Meta.Owner.Kind)
	}
	if b.Meta.Owner.ID != "" && b.Meta.Owner.ID != b.Spec.ProjectID {
		return fmt.Errorf("policybinding %q: owner.id %q does not match spec.projectId %q", b.Meta.Name, b.Meta.Owner.ID, b.Spec.ProjectID)
	}
	return rolebinding.ValidateSubjects(b.Meta.Name, b.Spec.Subjects)
}
