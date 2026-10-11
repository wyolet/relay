package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/wyolet/relay/auth/directory"
)

// The From functions render a stored row as a document, ids resolved back to names through names. An id names cannot resolve renders as itself.

func docMeta(m directory.Metadata) Metadata {
	return Metadata{ID: m.ID, Name: m.Name, DisplayName: m.DisplayName, Description: m.Description, Labels: m.Labels, Annotations: m.Annotations}
}

func (r *Registry) rendered(kind string, m directory.Metadata, attrs map[string]json.RawMessage, names Namer) (*Document, error) {
	fields, err := r.fromAttributes(kind, attrs, names)
	if err != nil {
		return nil, &Error{Kind: kind, Name: m.Name, Err: err}
	}
	return &Document{APIVersion: APIVersion, Kind: kind, Metadata: docMeta(m), Fields: fields}, nil
}

// FromTeam renders a team.
func (r *Registry) FromTeam(t *directory.Team, names Namer) (*Document, error) {
	d, err := r.rendered(KindTeam, t.Meta, t.Spec.Attributes, names)
	if err != nil {
		return nil, err
	}
	d.Team = &TeamSpec{Enabled: t.Spec.Enabled}
	return d, nil
}

// FromProject renders a project.
func (r *Registry) FromProject(p *directory.Project, names Namer) (*Document, error) {
	d, err := r.rendered(KindProject, p.Meta, p.Spec.Attributes, names)
	if err != nil {
		return nil, err
	}
	d.Project = &ProjectSpec{Team: nameOf(names, KindTeam, p.Spec.TeamID), Enabled: p.Spec.Enabled}
	return d, nil
}

// FromServiceAccount renders a service account.
func (r *Registry) FromServiceAccount(sa *directory.ServiceAccount, names Namer) (*Document, error) {
	d, err := r.rendered(KindServiceAccount, sa.Meta, sa.Spec.Attributes, names)
	if err != nil {
		return nil, err
	}
	d.ServiceAccount = &ServiceAccountSpec{Project: nameOf(names, KindProject, sa.Spec.ProjectID), Enabled: sa.Spec.Enabled}
	return d, nil
}

// FromRole renders a role.
func (r *Registry) FromRole(ro *directory.Role, names Namer) (*Document, error) {
	d, err := r.rendered(KindRole, ro.Meta, ro.Spec.Attributes, names)
	if err != nil {
		return nil, err
	}
	rules := make([]Rule, 0, len(ro.Spec.Rules))
	for _, rule := range ro.Spec.Rules {
		rules = append(rules, Rule{Kinds: rule.Kinds, Verbs: rule.Verbs})
	}
	d.Role = &RoleSpec{Rules: rules, Enabled: ro.Spec.Enabled}
	return d, nil
}

// FromGroup renders a group, members by username.
func (r *Registry) FromGroup(g *directory.Group, names Namer) (*Document, error) {
	d, err := r.rendered(KindGroup, g.Meta, g.Spec.Attributes, names)
	if err != nil {
		return nil, err
	}
	var members []string
	for _, id := range g.Spec.MemberIDs {
		members = append(members, nameOf(names, KindUser, id))
	}
	d.Group = &GroupSpec{Members: members, Enabled: g.Spec.Enabled}
	return d, nil
}

// FromRoleBinding renders a binding: the role by name, a team or project scope by name, subjects by name.
func (r *Registry) FromRoleBinding(b *directory.RoleBinding, names Namer) (*Document, error) {
	d, err := r.rendered(KindRoleBinding, b.Meta, b.Spec.Attributes, names)
	if err != nil {
		return nil, err
	}
	scope := Scope{Kind: b.Spec.Scope.Kind, Name: b.Spec.Scope.ID}
	switch scope.Kind {
	case directory.OwnerTeam:
		scope.Name = nameOf(names, KindTeam, b.Spec.Scope.ID)
	case directory.OwnerProject:
		scope.Name = nameOf(names, KindProject, b.Spec.Scope.ID)
	}
	subjects := make([]Subject, 0, len(b.Spec.Subjects))
	for _, sub := range b.Spec.Subjects {
		s := Subject{Kind: string(sub.Kind), Name: sub.Name}
		switch sub.Kind {
		case directory.SubjectUser:
			s.Name = nameOf(names, KindUser, sub.ID)
		case directory.SubjectServiceAccount:
			s.Name = nameOf(names, KindServiceAccount, sub.ID)
		}
		subjects = append(subjects, s)
	}
	d.RoleBinding = &RoleBindingSpec{Role: nameOf(names, KindRole, b.Spec.RoleID), Scope: scope, Subjects: subjects, Enabled: b.Spec.Enabled}
	return d, nil
}

// spec returns the document's built-in spec struct.
func (d *Document) spec() any {
	switch {
	case d.Team != nil:
		return d.Team
	case d.Project != nil:
		return d.Project
	case d.ServiceAccount != nil:
		return d.ServiceAccount
	case d.Role != nil:
		return d.Role
	case d.User != nil:
		return d.User
	case d.Group != nil:
		return d.Group
	case d.RoleBinding != nil:
		return d.RoleBinding
	}
	return nil
}

// Render writes docs as one multi-document YAML stream, product fields after the built-in spec fields.
func Render(docs []*Document) ([]byte, error) {
	var buf bytes.Buffer
	for i, d := range docs {
		if i > 0 {
			buf.WriteString("---\n")
		}
		node, err := documentNode(d)
		if err != nil {
			return nil, fmt.Errorf("manifest: render %s %q: %w", d.Kind, d.Metadata.Name, err)
		}
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(node); err != nil {
			return nil, fmt.Errorf("manifest: render %s %q: %w", d.Kind, d.Metadata.Name, err)
		}
		if err := enc.Close(); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func documentNode(d *Document) (*yaml.Node, error) {
	var meta, spec yaml.Node
	m := d.Metadata
	m.Owner = nil
	if err := meta.Encode(m); err != nil {
		return nil, err
	}
	if err := spec.Encode(d.spec()); err != nil {
		return nil, err
	}
	// An empty spec encodes in flow style, which would carry over to the product fields appended below.
	meta.Style, spec.Style = 0, 0
	names := make([]string, 0, len(d.Fields))
	for name := range d.Fields {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var v any
		if err := json.Unmarshal(d.Fields[name], &v); err != nil {
			return nil, fmt.Errorf("spec.%s: %w", name, err)
		}
		var value yaml.Node
		if err := value.Encode(v); err != nil {
			return nil, fmt.Errorf("spec.%s: %w", name, err)
		}
		spec.Content = append(spec.Content, scalar(name), &value)
	}
	return &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		scalar("apiVersion"), scalar(d.APIVersion),
		scalar("kind"), scalar(d.Kind),
		scalar("metadata"), &meta,
		scalar("spec"), &spec,
	}}, nil
}

func scalar(s string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: s} }
