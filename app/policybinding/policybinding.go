// Package policybinding is the domain layer for the PolicyBinding entity —
// it points a set of subjects inside one Project at one Policy. Several
// bindings may match a principal; the lowest priority wins, so a principal
// always resolves to exactly one Policy. Subjects may also name a host, an
// account, a credential, a team or a project.
//
// Authorization is app/authz's concern; this package validates shape only.
package policybinding

import (
	"fmt"
	"slices"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/rolebinding"
)

// DefaultPriority is applied to a binding that declares none. Lower wins.
const DefaultPriority = 100

// Subject kinds a PolicyBinding accepts beyond the RoleBinding ones: the upstream side (host, account, credential) and the tenancy scopes. A relay key is never a subject: it acts as its principal.
const (
	SubjectHost       rolebinding.SubjectKind = "host"
	SubjectAccount    rolebinding.SubjectKind = "account"
	SubjectCredential rolebinding.SubjectKind = "credential"
	SubjectTeam       rolebinding.SubjectKind = "team"
	SubjectProject    rolebinding.SubjectKind = "project"
)

// SubjectKinds is every kind a PolicyBinding subject may have.
var SubjectKinds = []rolebinding.SubjectKind{
	rolebinding.SubjectUser, rolebinding.SubjectGroup, rolebinding.SubjectServiceAccount,
	SubjectHost, SubjectAccount, SubjectCredential, SubjectTeam, SubjectProject,
}

// PolicyBinding attaches Spec.PolicyID to Spec.Subjects inside a Project.
type PolicyBinding struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`

	// SubjectKeys is Spec.Subjects rendered as match keys. Derived, so it
	// stays off the wire and out of manifests; the snapshot builder fills
	// it so the request path compares strings instead of building them.
	SubjectKeys []string `json:"-" yaml:"-"`
}

// IndexSubjects recomputes SubjectKeys. Called wherever a binding enters
// the snapshot; cheap, and never on the request path.
func (b *PolicyBinding) IndexSubjects() {
	keys := make([]string, len(b.Spec.Subjects))
	for i := range b.Spec.Subjects {
		keys[i] = b.Spec.Subjects[i].Key()
	}
	b.SubjectKeys = keys
}

// Spec carries the project, the policy, the tie-break priority, the
// subjects, and the enabled flag.
type Spec struct {
	ProjectID string `json:"projectId" yaml:"projectId" validate:"required,uuid"`
	PolicyID  string `json:"policyId"  yaml:"policyId"  validate:"required,uuid"`
	Priority  *int   `json:"priority,omitempty" yaml:"priority,omitempty" validate:"omitempty,gte=0,lte=10000"`
	// Subjects are not dived into: rolebinding.Subject's own tags admit only the RoleBinding kinds, so Validate checks each one against SubjectKinds.
	Subjects []rolebinding.Subject `json:"subjects"  yaml:"subjects"  validate:"required,min=1"`
	Enabled  *bool                 `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

// IsEnabled returns true when Enabled is unset or explicitly true.
func (b *PolicyBinding) IsEnabled() bool { return b.Spec.Enabled == nil || *b.Spec.Enabled }

// EffectivePriority reads an unset priority as the default, so a binding
// authored without one still orders against explicit values. The field is a
// pointer because 0 is a valid, highest-winning priority and must not read
// as absent.
func (b *PolicyBinding) EffectivePriority() int {
	if b.Spec.Priority == nil {
		return DefaultPriority
	}
	return *b.Spec.Priority
}

// StampOwner sets Meta.Owner to the binding's Project. Called by every
// write path so Owner never drifts from ProjectID.
func (b *PolicyBinding) StampOwner() {
	b.Meta.Owner = meta.Owner{Kind: meta.OwnerProject, ID: b.Spec.ProjectID}
}

// Validate runs intra-row rules via the shared meta.Validator and enforces:
//   - Owner.Kind is project; Owner.ID, when set, equals Spec.ProjectID.
//   - every subject has one of SubjectKinds, and an id-bearing one a UUID.
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
	for i := range b.Spec.Subjects {
		s := &b.Spec.Subjects[i]
		if !slices.Contains(SubjectKinds, s.Kind) {
			return fmt.Errorf("policybinding %q: subjects[%d].kind %q is not one of %v", b.Meta.Name, i, s.Kind, SubjectKinds)
		}
		if err := meta.Validator.Var(s.ID, "omitempty,uuid"); err != nil {
			return fmt.Errorf("policybinding %q: subjects[%d].id %q is not a UUID", b.Meta.Name, i, s.ID)
		}
	}
	return rolebinding.ValidateSubjects(b.Meta.Name, b.Spec.Subjects)
}
