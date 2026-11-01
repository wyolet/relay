// Package policybinding is the domain layer for the PolicyBinding entity —
// it points a set of subjects inside its owner's scope (the system, a team or
// a project) at one Policy. A subject with an id is held by at most one
// binding, except a user, who may be bound once per scope. Subjects may also
// name a host, an account, a credential, a team or a project.
//
// Authorization and the cross-row scope rules are app/authz's and
// app/refcheck's concern; this package validates shape only.
package policybinding

import (
	"errors"
	"fmt"
	"slices"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/rolebinding"
)

// Subject kinds a PolicyBinding accepts beyond the RoleBinding ones: the upstream side (host, account, credential) and the tenancy scopes. A relay key is never a subject: it acts as its principal.
const (
	SubjectHost       rolebinding.SubjectKind = "host"
	SubjectAccount    rolebinding.SubjectKind = "account"
	SubjectCredential rolebinding.SubjectKind = "credential"
	SubjectTeam       rolebinding.SubjectKind = "team"
	SubjectProject    rolebinding.SubjectKind = "project"
)

// SubjectKinds is every kind a PolicyBinding subject may have. A group is no rung of the ladder, so binding one would govern nobody.
var SubjectKinds = []rolebinding.SubjectKind{
	rolebinding.SubjectUser, rolebinding.SubjectServiceAccount,
	SubjectHost, SubjectAccount, SubjectCredential, SubjectTeam, SubjectProject,
}

// ErrGroupSubject refuses a group subject.
var ErrGroupSubject = errors.New("groups are not policy subjects in this version; bind the users, their team or their project instead")

// PolicyBinding attaches Spec.PolicyID to Spec.Subjects inside the scope Meta.Owner names.
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

// Spec carries the project, the policy, the subjects, and the enabled flag.
type Spec struct {
	// ProjectID is the owning project; empty when a team or the system owns the binding.
	ProjectID string `json:"projectId,omitempty" yaml:"projectId,omitempty" validate:"omitempty,uuid" doc:"Id of the owning project; set it for a project-owned binding (the owner follows it). Empty when a team or the system owns the binding."`
	PolicyID  string `json:"policyId"  yaml:"policyId"  validate:"required,uuid" doc:"Id of the policy the subjects are bound to."`
	// Subjects are not dived into: rolebinding.Subject's own tags admit only the RoleBinding kinds, so Validate checks each one against SubjectKinds.
	Subjects []rolebinding.Subject `json:"subjects"  yaml:"subjects"  validate:"required,min=1" doc:"Rungs the policy governs, each inside the binding owner's scope. A subject is held by at most one binding (a user once per scope); a second answers 409 duplicate_binding. Only a system-owned binding may name a host."`
	Enabled  *bool                 `json:"enabled,omitempty" yaml:"enabled,omitempty" doc:"False stops the binding from governing its subjects. Omitted means true."`
}

// IsEnabled returns true when Enabled is unset or explicitly true.
func (b *PolicyBinding) IsEnabled() bool { return b.Spec.Enabled == nil || *b.Spec.Enabled }

// StampOwner keeps Meta.Owner and Spec.ProjectID in step: a projectId makes
// the binding that project's whatever the owner said, and a project owner
// fills an empty projectId. Called by every write path. Without a projectId
// a team or system owner is kept.
func (b *PolicyBinding) StampOwner() {
	switch {
	case b.Spec.ProjectID != "":
		b.Meta.Owner = meta.Owner{Kind: meta.OwnerProject, ID: b.Spec.ProjectID}
	case b.Meta.Owner.Kind == meta.OwnerProject:
		b.Spec.ProjectID = b.Meta.Owner.ID
	}
}

// ScopeKey names the owner scope the way subjects are keyed ("team:<id>", "project:<id>"), "" for the system.
func (b *PolicyBinding) ScopeKey() string {
	if b.Meta.Owner.Kind == meta.OwnerSystem {
		return ""
	}
	return string(b.Meta.Owner.Kind) + ":" + b.Meta.Owner.ID
}

// Validate runs intra-row rules via the shared meta.Validator and enforces:
//   - Owner.Kind is system, team or project; a project owner equals
//     Spec.ProjectID, which is empty for the other two.
//   - every subject has one of SubjectKinds, and an id-bearing one a UUID.
//   - the subject rules shared with RoleBinding.
func (b *PolicyBinding) Validate() error {
	if err := meta.Validator.Struct(b); err != nil {
		return err
	}
	switch o := b.Meta.Owner; o.Kind {
	case meta.OwnerProject:
		if b.Spec.ProjectID == "" {
			return fmt.Errorf("policybinding %q: a project-owned binding needs spec.projectId", b.Meta.Name)
		}
		if o.ID != "" && o.ID != b.Spec.ProjectID {
			return fmt.Errorf("policybinding %q: owner.id %q does not match spec.projectId %q", b.Meta.Name, o.ID, b.Spec.ProjectID)
		}
	case meta.OwnerTeam, meta.OwnerSystem:
		if o.Kind == meta.OwnerTeam && meta.Validator.Var(o.ID, "required,uuid") != nil {
			return fmt.Errorf("policybinding %q: a team owner needs the team's id, got %q", b.Meta.Name, o.ID)
		}
		if o.Kind == meta.OwnerSystem && o.ID != "" {
			return fmt.Errorf("policybinding %q: a system owner has no id, got %q", b.Meta.Name, o.ID)
		}
		if b.Spec.ProjectID != "" {
			return fmt.Errorf("policybinding %q: spec.projectId must be empty for a %s-owned binding", b.Meta.Name, o.Kind)
		}
	default:
		return fmt.Errorf("policybinding %q: owner.kind must be system, team or project, got %q", b.Meta.Name, o.Kind)
	}
	for i := range b.Spec.Subjects {
		s := &b.Spec.Subjects[i]
		if s.Kind == rolebinding.SubjectGroup {
			return fmt.Errorf("policybinding %q: subjects[%d] (group %q): %w", b.Meta.Name, i, s.Name, ErrGroupSubject)
		}
		if !slices.Contains(SubjectKinds, s.Kind) {
			return fmt.Errorf("policybinding %q: subjects[%d].kind %q is not one of %v", b.Meta.Name, i, s.Kind, SubjectKinds)
		}
		if err := meta.Validator.Var(s.ID, "omitempty,uuid"); err != nil {
			return fmt.Errorf("policybinding %q: subjects[%d].id %q is not a UUID", b.Meta.Name, i, s.ID)
		}
	}
	return rolebinding.ValidateSubjects(b.Meta.Name, b.Spec.Subjects)
}
