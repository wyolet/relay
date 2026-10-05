package manifest

import (
	"fmt"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
)

// ---------------------------------------------------------------------------
// Role / RoleBinding / PolicyBinding
// ---------------------------------------------------------------------------

func ToRole(d RoleDTO, _ Resolver) (*role.Role, error) {
	m := d.Metadata.toMeta()
	// A Role is a rule set anyone may be bound to; ownership is relay's,
	// not the manifest author's.
	m.Owner = meta.Owner{Kind: meta.OwnerSystem}
	rules := make([]role.Rule, 0, len(d.Spec.Rules))
	for _, r := range d.Spec.Rules {
		rules = append(rules, role.Rule{Kinds: r.Kinds, Verbs: r.Verbs})
	}
	return &role.Role{
		Meta: m,
		Spec: role.Spec{Rules: rules, Enabled: d.Spec.Enabled},
	}, nil
}

func FromRole(r *role.Role, _ ReverseResolver) RoleDTO {
	rules := make([]RoleRuleDTO, 0, len(r.Spec.Rules))
	for _, rule := range r.Spec.Rules {
		rules = append(rules, RoleRuleDTO{Kinds: rule.Kinds, Verbs: rule.Verbs})
	}
	return RoleDTO{
		APIVersion: APIVersion,
		Kind:       "Role",
		Metadata:   metaToWire(r.Meta),
		Spec:       RoleSpec{Rules: rules, Enabled: r.Spec.Enabled},
	}
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

// ToRoleBinding resolves the role name, the scope target, and the subjects.
// Owner mirrors spec.scope, so it is re-derived rather than read from the
// wire form.
func ToRoleBinding(d RoleBindingDTO, idx Resolver) (*rolebinding.RoleBinding, error) {
	roleID, ok := idx.RoleID(d.Spec.Role)
	if !ok {
		return nil, refNotFound("rolebinding %q: role %q not found", d.Metadata.Name, d.Spec.Role)
	}
	scope := meta.Owner{Kind: d.Spec.Scope.Kind, ID: d.Spec.Scope.ref()}
	switch scope.Kind {
	case meta.OwnerTeam:
		id, ok := idx.TeamID(scope.ID)
		if !ok {
			return nil, refNotFound("rolebinding %q: team %q not found", d.Metadata.Name, scope.ID)
		}
		scope.ID = id
	case meta.OwnerProject:
		id, ok := idx.ProjectID(scope.ID)
		if !ok {
			return nil, refNotFound("rolebinding %q: project %q not found", d.Metadata.Name, scope.ID)
		}
		scope.ID = id
	}
	subjects, err := toSubjects(d.Metadata.Name, d.Spec.Subjects, idx)
	if err != nil {
		return nil, err
	}
	b := &rolebinding.RoleBinding{
		Meta: d.Metadata.toMeta(),
		Spec: rolebinding.Spec{
			RoleID:   roleID,
			Scope:    scope,
			Subjects: subjects,
			Enabled:  d.Spec.Enabled,
		},
	}
	b.StampOwner()
	return b, nil
}

func FromRoleBinding(b *rolebinding.RoleBinding, rev ReverseResolver) RoleBindingDTO {
	roleName, _ := rev.RoleName(b.Spec.RoleID)
	if roleName == "" {
		roleName = b.Spec.RoleID
	}
	scope := WireOwner{Kind: b.Spec.Scope.Kind, Name: b.Spec.Scope.ID}
	switch b.Spec.Scope.Kind {
	case meta.OwnerTeam:
		if name, ok := rev.TeamName(b.Spec.Scope.ID); ok {
			scope.Name = name
		}
	case meta.OwnerProject:
		if name, ok := rev.ProjectName(b.Spec.Scope.ID); ok {
			scope.Name = name
		}
	}
	wm := metaToWire(b.Meta)
	wm.Owner.Name = scope.Name
	return RoleBindingDTO{
		APIVersion: APIVersion,
		Kind:       "RoleBinding",
		Metadata:   wm,
		Spec: RoleBindingSpec{
			Role:     roleName,
			Scope:    scope,
			Subjects: fromSubjects(b.Spec.Subjects, rev),
			Enabled:  b.Spec.Enabled,
		},
	}
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
