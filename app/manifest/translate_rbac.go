package manifest

import (
	"fmt"

	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
)

// ---------------------------------------------------------------------------
// Role / RoleBinding / PolicyBinding
// ---------------------------------------------------------------------------

// ToRole translates through the auth module, which makes every role system-owned: a role is a rule set anyone may be bound to, so ownership is relay's, not the manifest author's.
func ToRole(d RoleDTO, idx Resolver) (*role.Role, error) {
	row, err := AuthManifests.ToRole(roleDoc(d), authRefs{idx})
	if err != nil {
		return nil, fromAuthError(err)
	}
	return role.FromDirectory(row), nil
}

func FromRole(r *role.Role, rev ReverseResolver) RoleDTO {
	d, _ := fromAuthDoc(rendered(AuthManifests.FromRole(r.ToDirectory(), authNames{rev})))
	dto := *d.Role
	dto.APIVersion, dto.Metadata = APIVersion, metaToWire(r.Meta)
	return dto
}

// toSubjects resolves named subjects to the form the snapshot indexes:
// users and service accounts by id, groups by name.
func toSubjects(name string, subjects []SubjectDTO, idx Resolver) ([]rolebinding.Subject, error) {
	out := make([]rolebinding.Subject, 0, len(subjects))
	for _, sub := range subjects {
		switch rolebinding.SubjectKind(sub.Kind) {
		case rolebinding.SubjectGroup:
			out = append(out, rolebinding.Subject{Kind: rolebinding.SubjectGroup, Name: sub.Name})
		case rolebinding.SubjectUser:
			id, ok := idx.UserID(sub.Name)
			if !ok {
				return nil, refNotFound("binding %q: user %q not found", name, sub.Name)
			}
			out = append(out, rolebinding.Subject{Kind: rolebinding.SubjectUser, ID: id})
		case rolebinding.SubjectServiceAccount:
			id, ok := idx.ServiceAccountID(sub.Name)
			if !ok {
				return nil, refNotFound("binding %q: service account %q not found", name, sub.Name)
			}
			out = append(out, rolebinding.Subject{Kind: rolebinding.SubjectServiceAccount, ID: id})
		default:
			return nil, fmt.Errorf("binding %q: subject.kind must be user, group, or serviceaccount, got %q", name, sub.Kind)
		}
	}
	return out, nil
}

func fromSubjects(subjects []rolebinding.Subject, rev ReverseResolver) []SubjectDTO {
	out := make([]SubjectDTO, 0, len(subjects))
	for _, sub := range subjects {
		wire := SubjectDTO{Kind: string(sub.Kind), Name: sub.Name}
		switch sub.Kind {
		case rolebinding.SubjectUser:
			if n, ok := rev.Username(sub.ID); ok {
				wire.Name = n
			} else {
				wire.Name = sub.ID
			}
		case rolebinding.SubjectServiceAccount:
			if n, ok := rev.ServiceAccountName(sub.ID); ok {
				wire.Name = n
			} else {
				wire.Name = sub.ID
			}
		}
		out = append(out, wire)
	}
	return out
}

// ToRoleBinding resolves the role name, the scope target, and the subjects
// through the auth module. Owner mirrors spec.scope, so it is re-derived
// rather than read from the wire form.
func ToRoleBinding(d RoleBindingDTO, idx Resolver) (*rolebinding.RoleBinding, error) {
	row, err := AuthManifests.ToRoleBinding(roleBindingDoc(d), authRefs{idx})
	if err != nil {
		return nil, fromAuthError(err)
	}
	return rolebinding.FromDirectory(row), nil
}

func FromRoleBinding(b *rolebinding.RoleBinding, rev ReverseResolver) RoleBindingDTO {
	d, _ := fromAuthDoc(rendered(AuthManifests.FromRoleBinding(b.ToDirectory(), authNames{rev})))
	dto := *d.RoleBinding
	dto.APIVersion, dto.Metadata = APIVersion, metaToWire(b.Meta)
	dto.Metadata.Owner.Name = dto.Spec.Scope.Name
	return dto
}

// ToPolicyBinding resolves the project, the policy, and the subjects. Owner
// mirrors spec.project.
func ToPolicyBinding(d PolicyBindingDTO, idx Resolver) (*policybinding.PolicyBinding, error) {
	projectID, ok := idx.ProjectID(d.Spec.Project)
	if !ok {
		return nil, refNotFound("policybinding %q: project %q not found", d.Metadata.Name, d.Spec.Project)
	}
	policyID, ok := idx.PolicyID(d.Spec.Policy)
	if !ok {
		return nil, refNotFound("policybinding %q: policy %q not found", d.Metadata.Name, d.Spec.Policy)
	}
	subjects, err := toSubjects(d.Metadata.Name, d.Spec.Subjects, idx)
	if err != nil {
		return nil, err
	}
	priority := d.Spec.Priority
	if priority == nil {
		// The store and control API write this default, so an omitted
		// priority must match or apply reports an update every run. An
		// explicit 0 is a real priority and is kept.
		def := policybinding.DefaultPriority
		priority = &def
	}
	b := &policybinding.PolicyBinding{
		Meta: d.Metadata.toMeta(),
		Spec: policybinding.Spec{
			ProjectID: projectID,
			PolicyID:  policyID,
			Priority:  priority,
			Subjects:  subjects,
			Enabled:   d.Spec.Enabled,
		},
	}
	b.StampOwner()
	return b, nil
}

func FromPolicyBinding(b *policybinding.PolicyBinding, rev ReverseResolver) PolicyBindingDTO {
	projectName, _ := rev.ProjectName(b.Spec.ProjectID)
	if projectName == "" {
		projectName = b.Spec.ProjectID
	}
	policyName, _ := rev.PolicyName(b.Spec.PolicyID)
	if policyName == "" {
		policyName = b.Spec.PolicyID
	}
	wm := metaToWire(b.Meta)
	wm.Owner.Name = projectName
	effectivePriority := b.EffectivePriority()
	return PolicyBindingDTO{
		APIVersion: APIVersion,
		Kind:       "PolicyBinding",
		Metadata:   wm,
		Spec: PolicyBindingSpec{
			Project:  projectName,
			Policy:   policyName,
			Priority: &effectivePriority,
			Subjects: fromSubjects(b.Spec.Subjects, rev),
			Enabled:  b.Spec.Enabled,
		},
	}
}
