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

// subjectIDs returns the name→id lookup and the wording for each id-bearing
// policy-binding subject kind; groups carry a name and have none.
func subjectIDs(idx Resolver, kind rolebinding.SubjectKind) (func(string) (string, bool), string) {
	switch kind {
	case rolebinding.SubjectUser:
		return idx.UserID, "user"
	case rolebinding.SubjectServiceAccount:
		return idx.ServiceAccountID, "service account"
	case policybinding.SubjectHost:
		return idx.HostID, "host"
	case policybinding.SubjectAccount:
		return idx.AccountID, "account"
	case policybinding.SubjectCredential:
		return idx.HostKeyID, "credential"
	case policybinding.SubjectTeam:
		return idx.TeamID, "team"
	case policybinding.SubjectProject:
		return idx.ProjectID, "project"
	}
	return nil, ""
}

func subjectNames(rev ReverseResolver, kind rolebinding.SubjectKind) func(string) (string, bool) {
	switch kind {
	case rolebinding.SubjectUser:
		return rev.Username
	case rolebinding.SubjectServiceAccount:
		return rev.ServiceAccountName
	case policybinding.SubjectHost:
		return rev.HostName
	case policybinding.SubjectAccount:
		return rev.AccountName
	case policybinding.SubjectCredential:
		return rev.HostKeyName
	case policybinding.SubjectTeam:
		return rev.TeamName
	case policybinding.SubjectProject:
		return rev.ProjectName
	}
	return nil
}

// toSubjects resolves named subjects to the form the snapshot indexes: every
// kind by id except groups, which keep their name.
func toSubjects(name string, subjects []PolicySubjectDTO, idx Resolver) ([]rolebinding.Subject, error) {
	out := make([]rolebinding.Subject, 0, len(subjects))
	for _, sub := range subjects {
		kind := rolebinding.SubjectKind(sub.Kind)
		if kind == rolebinding.SubjectGroup {
			out = append(out, rolebinding.Subject{Kind: kind, Name: sub.Name})
			continue
		}
		lookup, what := subjectIDs(idx, kind)
		if lookup == nil {
			return nil, fmt.Errorf("binding %q: subject.kind must be one of %v, got %q", name, policybinding.SubjectKinds, sub.Kind)
		}
		id, ok := lookup(sub.Name)
		if !ok {
			return nil, refNotFound("binding %q: %s %q not found", name, what, sub.Name)
		}
		out = append(out, rolebinding.Subject{Kind: kind, ID: id})
	}
	return out, nil
}

func fromSubjects(subjects []rolebinding.Subject, rev ReverseResolver) []PolicySubjectDTO {
	out := make([]PolicySubjectDTO, 0, len(subjects))
	for _, sub := range subjects {
		wire := PolicySubjectDTO{Kind: string(sub.Kind), Name: sub.Name}
		if names := subjectNames(rev, sub.Kind); names != nil {
			wire.Name = nameOr(names, sub.ID)
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

// ToPolicyBinding resolves the owner, the project, the policy, and the
// subjects. A spec.project makes the binding that project's; without one,
// metadata.owner names a team or the system.
func ToPolicyBinding(d PolicyBindingDTO, idx Resolver) (*policybinding.PolicyBinding, error) {
	var projectID string
	if d.Spec.Project != "" {
		id, ok := idx.ProjectID(d.Spec.Project)
		if !ok {
			return nil, refNotFound("policybinding %q: project %q not found", d.Metadata.Name, d.Spec.Project)
		}
		projectID = id
	}
	m := d.Metadata.toMeta()
	resolveScopeOwner(&m.Owner, idx)
	policyID, ok := idx.PolicyID(d.Spec.Policy)
	if !ok {
		return nil, refNotFound("policybinding %q: policy %q not found", d.Metadata.Name, d.Spec.Policy)
	}
	subjects, err := toSubjects(d.Metadata.Name, d.Spec.Subjects, idx)
	if err != nil {
		return nil, err
	}
	b := &policybinding.PolicyBinding{
		Meta: m,
		Spec: policybinding.Spec{
			ProjectID: projectID,
			PolicyID:  policyID,
			Subjects:  subjects,
			Enabled:   d.Spec.Enabled,
		},
	}
	b.StampOwner()
	return b, nil
}

func FromPolicyBinding(b *policybinding.PolicyBinding, rev ReverseResolver) PolicyBindingDTO {
	var projectName string
	if b.Spec.ProjectID != "" {
		projectName, _ = rev.ProjectName(b.Spec.ProjectID)
		if projectName == "" {
			projectName = b.Spec.ProjectID
		}
	}
	policyName, _ := rev.PolicyName(b.Spec.PolicyID)
	if policyName == "" {
		policyName = b.Spec.PolicyID
	}
	wm := metaToWire(b.Meta)
	switch b.Meta.Owner.Kind {
	case meta.OwnerProject:
		wm.Owner.Name = projectName
	case meta.OwnerTeam:
		if name, ok := rev.TeamName(b.Meta.Owner.ID); ok {
			wm.Owner.Name = name
		}
	}
	return PolicyBindingDTO{
		APIVersion: APIVersion,
		Kind:       "PolicyBinding",
		Metadata:   wm,
		Spec: PolicyBindingSpec{
			Project:  projectName,
			Policy:   policyName,
			Subjects: fromSubjects(b.Spec.Subjects, rev),
			Enabled:  b.Spec.Enabled,
		},
	}
}
