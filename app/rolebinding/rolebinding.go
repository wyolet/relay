// Package rolebinding is the domain layer for the RoleBinding entity — it
// grants one Role to a set of subjects at one scope (global, a Team, or a
// Project). A binding at scope S applies to every resource whose scope
// chain contains S.
//
// Authorization is app/authz's concern; this package validates shape only.
package rolebinding

import (
	"fmt"

	"github.com/wyolet/relay/app/meta"
)

// SubjectKind enumerates what a binding can name.
type SubjectKind string

const (
	SubjectUser           SubjectKind = "user"
	SubjectGroup          SubjectKind = "group"
	SubjectServiceAccount SubjectKind = "serviceaccount"
)

// Subject is one grantee. User and service-account subjects carry an id;
// group subjects carry a name, because an IdP group has no row to point at
// and a local group of the same name unions with it.
type Subject struct {
	Kind SubjectKind `json:"kind" yaml:"kind" validate:"required,oneof=user group serviceaccount"`
	ID   string      `json:"id,omitempty"   yaml:"id,omitempty"   validate:"omitempty,uuid"`
	Name string      `json:"name,omitempty" yaml:"name,omitempty"`
}

// Key renders the subject as the string the snapshot indexes it under:
// "<kind>:<id-or-name>".
func (s *Subject) Key() string {
	if s.Kind == SubjectGroup {
		return string(s.Kind) + ":" + s.Name
	}
	return string(s.Kind) + ":" + s.ID
}

// RoleBinding grants Spec.RoleID to Spec.Subjects at Spec.Scope.
type RoleBinding struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec carries the role, the scope, the subjects, and the enabled flag.
// Scope is a meta.Owner: {system} is global, {team,id} and {project,id}
// name a tenancy row. Meta.Owner mirrors it.
type Spec struct {
	RoleID   string     `json:"roleId"   yaml:"roleId"   validate:"required,uuid"`
	Scope    meta.Owner `json:"scope"    yaml:"scope"    validate:"required"`
	Subjects []Subject  `json:"subjects" yaml:"subjects" validate:"required,min=1,dive"`
	Enabled  *bool      `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

// IsEnabled returns true when Enabled is unset or explicitly true.
func (b *RoleBinding) IsEnabled() bool { return b.Spec.Enabled == nil || *b.Spec.Enabled }

// StampOwner sets Meta.Owner to the binding's scope. Called by every write
// path so Owner never drifts from Spec.Scope.
func (b *RoleBinding) StampOwner() { b.Meta.Owner = b.Spec.Scope }

// Validate runs intra-row rules via the shared meta.Validator and enforces:
//   - Scope.Kind is system, team, or project; a non-system scope carries an id
//     and a system scope carries none.
//   - Meta.Owner equals Spec.Scope.
//   - subjects are distinct, group subjects carry a name and the others an id.
func (b *RoleBinding) Validate() error {
	if err := meta.Validator.Struct(b); err != nil {
		return err
	}
	switch b.Spec.Scope.Kind {
	case meta.OwnerSystem:
		if b.Spec.Scope.ID != "" {
			return fmt.Errorf("rolebinding %q: global scope carries no id", b.Meta.Name)
		}
	case meta.OwnerTeam, meta.OwnerProject:
		if b.Spec.Scope.ID == "" {
			return fmt.Errorf("rolebinding %q: scope.kind %q requires an id", b.Meta.Name, b.Spec.Scope.Kind)
		}
	default:
		return fmt.Errorf("rolebinding %q: scope.kind must be system, team, or project, got %q", b.Meta.Name, b.Spec.Scope.Kind)
	}
	if b.Meta.Owner != b.Spec.Scope {
		return fmt.Errorf("rolebinding %q: owner %s/%s does not mirror scope %s/%s",
			b.Meta.Name, b.Meta.Owner.Kind, b.Meta.Owner.ID, b.Spec.Scope.Kind, b.Spec.Scope.ID)
	}
	return ValidateSubjects(b.Meta.Name, b.Spec.Subjects)
}

// ValidateSubjects enforces the subject rules shared by RoleBinding and
// PolicyBinding: exactly one identifier per subject, keyed by kind, and no
// duplicates within the list.
func ValidateSubjects(name string, subjects []Subject) error {
	seen := make(map[string]struct{}, len(subjects))
	for i := range subjects {
		s := &subjects[i]
		if s.Kind == SubjectGroup {
			if s.Name == "" || s.ID != "" {
				return fmt.Errorf("%q: group subject names a group, it carries no id", name)
			}
		} else if s.ID == "" || s.Name != "" {
			return fmt.Errorf("%q: %s subject carries an id, not a name", name, s.Kind)
		}
		key := s.Key()
		if _, dup := seen[key]; dup {
			return fmt.Errorf("%q: duplicate subject %q", name, key)
		}
		seen[key] = struct{}{}
	}
	return nil
}
