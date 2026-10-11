package manifest

import (
	"fmt"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/rbac"
)

// The To functions translate a document into the row the directory stores, resolving every name through refs (kinds as in Kinds, plus whatever product kinds the product's fields ask for). They check references, not the row: the directory validates the row when it is written.

func rowMeta(d *Document) directory.Metadata {
	return directory.Metadata{
		ID: d.Metadata.ID, Name: d.Metadata.Name, DisplayName: d.Metadata.DisplayName, Description: d.Metadata.Description,
		Labels: d.Metadata.Labels, Annotations: d.Metadata.Annotations,
	}
}

// systemOwned is the owner of a Team, Role or Group: the row names a scope or a grant, so an owner taken from the document would let its author inherit what is granted through it.
func systemOwned(m directory.Metadata) directory.Metadata {
	m.Owner.Kind, m.Owner.ID = directory.OwnerSystem, ""
	return m
}

func wrongKind(d *Document, want string) error {
	return docError(d, "kind", fmt.Errorf("want a %s document", want))
}

// ToTeam translates a Team document.
func (r *Registry) ToTeam(d *Document, refs Resolver) (*directory.Team, error) {
	if d.Team == nil {
		return nil, wrongKind(d, KindTeam)
	}
	attrs, err := r.toAttributes(d, refs)
	if err != nil {
		return nil, err
	}
	return &directory.Team{Meta: systemOwned(rowMeta(d)), Spec: directory.TeamSpec{Enabled: d.Team.Enabled, Attributes: attrs}}, nil
}

// ToProject translates a Project document; spec.team must resolve.
func (r *Registry) ToProject(d *Document, refs Resolver) (*directory.Project, error) {
	if d.Project == nil {
		return nil, wrongKind(d, KindProject)
	}
	teamID, ok := lookup(refs, KindTeam, d.Project.Team)
	if !ok {
		return nil, docError(d, "spec.team", &RefError{Kind: "team", Name: d.Project.Team})
	}
	attrs, err := r.toAttributes(d, refs)
	if err != nil {
		return nil, err
	}
	p := &directory.Project{Meta: rowMeta(d), Spec: directory.ProjectSpec{TeamID: teamID, Enabled: d.Project.Enabled, Attributes: attrs}}
	p.StampOwner()
	return p, nil
}

// ToServiceAccount translates a ServiceAccount document; spec.project must resolve.
func (r *Registry) ToServiceAccount(d *Document, refs Resolver) (*directory.ServiceAccount, error) {
	if d.ServiceAccount == nil {
		return nil, wrongKind(d, KindServiceAccount)
	}
	projectID, ok := lookup(refs, KindProject, d.ServiceAccount.Project)
	if !ok {
		return nil, docError(d, "spec.project", &RefError{Kind: "project", Name: d.ServiceAccount.Project})
	}
	attrs, err := r.toAttributes(d, refs)
	if err != nil {
		return nil, err
	}
	sa := &directory.ServiceAccount{Meta: rowMeta(d), Spec: directory.ServiceAccountSpec{ProjectID: projectID, Enabled: d.ServiceAccount.Enabled, Attributes: attrs}}
	sa.StampOwner()
	return sa, nil
}

// ToRole translates a Role document. The rules were checked against the vocabulary when the document was parsed.
func (r *Registry) ToRole(d *Document, refs Resolver) (*directory.Role, error) {
	if d.Role == nil {
		return nil, wrongKind(d, KindRole)
	}
	rules := make([]rbac.Rule, 0, len(d.Role.Rules))
	for _, rule := range d.Role.Rules {
		rules = append(rules, rbac.Rule{Kinds: rule.Kinds, Verbs: rule.Verbs})
	}
	attrs, err := r.toAttributes(d, refs)
	if err != nil {
		return nil, err
	}
	return &directory.Role{Meta: systemOwned(rowMeta(d)), Spec: directory.RoleSpec{Rules: rules, Enabled: d.Role.Enabled, Attributes: attrs}}, nil
}

// ToGroup translates a Group document; every member username must resolve.
func (r *Registry) ToGroup(d *Document, refs Resolver) (*directory.Group, error) {
	if d.Group == nil {
		return nil, wrongKind(d, KindGroup)
	}
	var memberIDs []string
	for i, username := range d.Group.Members {
		id, ok := lookup(refs, KindUser, username)
		if !ok {
			return nil, docError(d, item("spec.members", i), &RefError{Kind: "user", Name: username})
		}
		memberIDs = append(memberIDs, id)
	}
	attrs, err := r.toAttributes(d, refs)
	if err != nil {
		return nil, err
	}
	return &directory.Group{Meta: systemOwned(rowMeta(d)), Spec: directory.GroupSpec{MemberIDs: memberIDs, Enabled: d.Group.Enabled, Attributes: attrs}}, nil
}

// ToRoleBinding translates a RoleBinding document: the role, a team or project scope, and the user and service-account subjects must resolve; group subjects stay names.
func (r *Registry) ToRoleBinding(d *Document, refs Resolver) (*directory.RoleBinding, error) {
	s := d.RoleBinding
	if s == nil {
		return nil, wrongKind(d, KindRoleBinding)
	}
	roleID, ok := lookup(refs, KindRole, s.Role)
	if !ok {
		return nil, docError(d, "spec.role", &RefError{Kind: "role", Name: s.Role})
	}
	scope := rbac.Scope{Kind: s.Scope.Kind, ID: s.Scope.ref()}
	switch scope.Kind {
	case directory.OwnerTeam, directory.OwnerProject:
		kind, what := KindTeam, "team"
		if scope.Kind == directory.OwnerProject {
			kind, what = KindProject, "project"
		}
		id, ok := lookup(refs, kind, scope.ID)
		if !ok {
			return nil, docError(d, "spec.scope", &RefError{Kind: what, Name: scope.ID})
		}
		scope.ID = id
	}
	subjects, err := toSubjects(d, refs)
	if err != nil {
		return nil, err
	}
	attrs, err := r.toAttributes(d, refs)
	if err != nil {
		return nil, err
	}
	b := &directory.RoleBinding{Meta: rowMeta(d), Spec: directory.RoleBindingSpec{
		RoleID: roleID, Scope: scope, Subjects: subjects, Enabled: s.Enabled, Attributes: attrs,
	}}
	b.StampOwner()
	return b, nil
}

func toSubjects(d *Document, refs Resolver) ([]directory.Subject, error) {
	out := make([]directory.Subject, 0, len(d.RoleBinding.Subjects))
	for i, sub := range d.RoleBinding.Subjects {
		path := item("spec.subjects", i)
		switch directory.SubjectKind(sub.Kind) {
		case directory.SubjectGroup:
			out = append(out, directory.Subject{Kind: directory.SubjectGroup, Name: sub.Name})
		case directory.SubjectUser:
			id, ok := lookup(refs, KindUser, sub.Name)
			if !ok {
				return nil, docError(d, path, &RefError{Kind: "user", Name: sub.Name})
			}
			out = append(out, directory.Subject{Kind: directory.SubjectUser, ID: id})
		case directory.SubjectServiceAccount:
			id, ok := lookup(refs, KindServiceAccount, sub.Name)
			if !ok {
				return nil, docError(d, path, &RefError{Kind: "service account", Name: sub.Name})
			}
			out = append(out, directory.Subject{Kind: directory.SubjectServiceAccount, ID: id})
		default:
			return nil, docError(d, path+".kind", fmt.Errorf("subject.kind must be user, group, or serviceaccount, got %q", sub.Kind))
		}
	}
	return out, nil
}
