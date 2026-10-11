package directory

import (
	"encoding/json"
	"fmt"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/rbac"
)

// SubjectKind is what a binding subject names.
type SubjectKind string

const (
	SubjectUser           SubjectKind = "user"
	SubjectGroup          SubjectKind = "group"
	SubjectServiceAccount SubjectKind = "serviceaccount"
)

// AllAuthenticated is the reserved subject every authenticated principal acts under. It is stored as a group subject so a binding to it needs no membership; its Key is "group:system:authenticated", which a product adds to every principal's subjects.
var AllAuthenticated = Subject{Kind: SubjectGroup, Name: ReservedGroupPrefix + "authenticated"}

// Subject is one grantee. User and service-account subjects carry an id; a group subject carries a name, because a group asserted by an identity provider has no row to point at and unions with a stored group of the same name.
type Subject struct {
	Kind SubjectKind
	ID   string
	Name string
}

// Key renders the subject as "<kind>:<id or name>", the string a binding source indexes it under and a principal lists in its subjects.
func (s Subject) Key() string {
	if s.Kind == SubjectGroup {
		return string(s.Kind) + ":" + s.Name
	}
	return string(s.Kind) + ":" + s.ID
}

// RoleBinding grants a role to its subjects at one scope; it applies to every resource whose scope chain holds that scope. Its owner is always its scope.
type RoleBinding struct {
	Meta Metadata
	Spec RoleBindingSpec
}

// RoleBindingSpec holds the role, the scope, the subjects and the enabled flag.
type RoleBindingSpec struct {
	RoleID string
	// Scope kinds are opaque; Global is the global scope and carries no id, any other kind needs one.
	Scope    rbac.Scope
	Subjects []Subject
	// Enabled nil means enabled.
	Enabled *bool
	// Attributes are product fields stored next to the spec, keyed by field name; the module round-trips them untouched.
	Attributes map[string]json.RawMessage
}

// IsEnabled reports whether the binding is enabled.
func (b *RoleBinding) IsEnabled() bool { return enabled(b.Spec.Enabled) }

// StampOwner sets the owner to the binding's scope, so it never drifts from it.
func (b *RoleBinding) StampOwner() { b.Meta.Owner = authz.Owner(b.Spec.Scope) }

func (b *RoleBinding) validate() error {
	if err := validateMeta("role binding", &b.Meta); err != nil {
		return err
	}
	if b.Spec.RoleID == "" {
		return fmt.Errorf("%w: role binding %q: role id is required", ErrInvalid, b.Meta.Name)
	}
	switch {
	case b.Spec.Scope.Kind == "":
		return fmt.Errorf("%w: role binding %q: scope kind is required", ErrInvalid, b.Meta.Name)
	case b.Spec.Scope.Kind == OwnerSystem && b.Spec.Scope.ID != "":
		return fmt.Errorf("%w: role binding %q: the global scope carries no id", ErrInvalid, b.Meta.Name)
	case b.Spec.Scope.Kind != OwnerSystem && b.Spec.Scope.ID == "":
		return fmt.Errorf("%w: role binding %q: scope kind %q requires an id", ErrInvalid, b.Meta.Name, b.Spec.Scope.Kind)
	}
	if len(b.Spec.Subjects) == 0 {
		return fmt.Errorf("%w: role binding %q: at least one subject is required", ErrInvalid, b.Meta.Name)
	}
	return validateSubjects(b.Meta.Name, b.Spec.Subjects)
}

// validateSubjects requires exactly one identifier per subject, chosen by kind, and no duplicates.
func validateSubjects(name string, subjects []Subject) error {
	seen := make(map[string]struct{}, len(subjects))
	for _, s := range subjects {
		switch s.Kind {
		case SubjectGroup:
			if s.Name == "" || s.ID != "" {
				return fmt.Errorf("%w: role binding %q: a group subject carries a name, not an id", ErrInvalid, name)
			}
		case SubjectUser, SubjectServiceAccount:
			if s.ID == "" || s.Name != "" {
				return fmt.Errorf("%w: role binding %q: a %s subject carries an id, not a name", ErrInvalid, name, s.Kind)
			}
		default:
			return fmt.Errorf("%w: role binding %q: unknown subject kind %q", ErrInvalid, name, s.Kind)
		}
		if _, dup := seen[s.Key()]; dup {
			return fmt.Errorf("%w: role binding %q: duplicate subject %q", ErrInvalid, name, s.Key())
		}
		seen[s.Key()] = struct{}{}
	}
	return nil
}
