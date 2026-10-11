package rolebinding

import (
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/rbac"
)

// ToDirectory is b as the directory stores it.
func (b *RoleBinding) ToDirectory() *directory.RoleBinding {
	var subjects []directory.Subject
	if b.Spec.Subjects != nil {
		subjects = make([]directory.Subject, len(b.Spec.Subjects))
		for i, s := range b.Spec.Subjects {
			subjects[i] = directory.Subject{Kind: directory.SubjectKind(s.Kind), ID: s.ID, Name: s.Name}
		}
	}
	return &directory.RoleBinding{
		Meta: meta.ToDirectory(b.Meta),
		Spec: directory.RoleBindingSpec{
			RoleID:   b.Spec.RoleID,
			Scope:    rbac.Scope{Kind: string(b.Spec.Scope.Kind), ID: b.Spec.Scope.ID},
			Subjects: subjects,
			Enabled:  b.Spec.Enabled,
		},
	}
}

// FromDirectory is a directory row as a RoleBinding.
func FromDirectory(r *directory.RoleBinding) *RoleBinding {
	var subjects []Subject
	if r.Spec.Subjects != nil {
		subjects = make([]Subject, len(r.Spec.Subjects))
		for i, s := range r.Spec.Subjects {
			subjects[i] = Subject{Kind: SubjectKind(s.Kind), ID: s.ID, Name: s.Name}
		}
	}
	b := &RoleBinding{
		Meta: meta.FromDirectory(r.Meta),
		Spec: Spec{
			RoleID:   r.Spec.RoleID,
			Scope:    meta.Owner{Kind: meta.OwnerKind(r.Spec.Scope.Kind), ID: r.Spec.Scope.ID},
			Subjects: subjects,
			Enabled:  r.Spec.Enabled,
		},
	}
	b.StampOwner()
	return b
}
