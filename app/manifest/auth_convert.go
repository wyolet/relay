package manifest

import (
	"encoding/json"
	"fmt"

	"github.com/wyolet/relay/app/meta"
	authmanifest "github.com/wyolet/relay/auth/manifest"
)

// These convert between relay's wire DTOs for the module's kinds and the module's documents. They carry every field across; the module owns what the fields mean.

func wireMetaOf(m authmanifest.Metadata) WireMeta {
	return WireMeta{ID: m.ID, Name: m.Name, DisplayName: m.DisplayName, Description: m.Description, Labels: m.Labels, Annotations: m.Annotations}
}

// docMetaOf leaves the wire owner behind: the module derives every owner of these kinds, as relay's translate always did.
func docMetaOf(w WireMeta) authmanifest.Metadata {
	return authmanifest.Metadata{ID: w.ID, Name: w.Name, DisplayName: w.DisplayName, Description: w.Description, Labels: w.Labels, Annotations: w.Annotations}
}

func authDoc(apiVersion, kind string, w WireMeta) *authmanifest.Document {
	return &authmanifest.Document{APIVersion: apiVersion, Kind: kind, Metadata: docMetaOf(w)}
}

// setField stores a relay spec field on the document as JSON; ok false leaves it out, as omitempty did.
func setField(d *authmanifest.Document, name string, v any, ok bool) {
	if !ok {
		return
	}
	raw, err := json.Marshal(v)
	if err != nil {
		// The relay fields are plain strings and structs of strings, which always marshal.
		return
	}
	if d.Fields == nil {
		d.Fields = map[string]json.RawMessage{}
	}
	d.Fields[name] = raw
}

// budgetOf reads the budget field strictly, so a typo inside it fails at parse like any other field.
func budgetOf(d *authmanifest.Document) (*BudgetDTO, error) {
	raw, ok := d.Fields["budget"]
	if !ok {
		return nil, nil
	}
	var b *BudgetDTO
	if err := strictJSON(raw, &b); err != nil {
		return nil, fmt.Errorf("spec.budget: %w", err)
	}
	return b, nil
}

func policyOf(d *authmanifest.Document) (string, error) {
	raw, ok := d.Fields["policy"]
	if !ok {
		return "", nil
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil {
		return "", fmt.Errorf("spec.policy: %w", err)
	}
	return name, nil
}

// fromAuthDoc wraps a decoded module document in relay's DTO for its kind.
func fromAuthDoc(d *authmanifest.Document) (Document, error) {
	wm := wireMetaOf(d.Metadata)
	switch d.Kind {
	case authmanifest.KindTeam:
		b, err := budgetOf(d)
		if err != nil {
			return Document{}, err
		}
		return Document{Team: &TeamDTO{APIVersion: d.APIVersion, Kind: d.Kind, Metadata: wm, Spec: TeamSpec{Enabled: d.Team.Enabled, Budget: b}}}, nil
	case authmanifest.KindProject:
		b, err := budgetOf(d)
		if err != nil {
			return Document{}, err
		}
		return Document{Project: &ProjectDTO{APIVersion: d.APIVersion, Kind: d.Kind, Metadata: wm,
			Spec: ProjectSpec{Team: d.Project.Team, Enabled: d.Project.Enabled, Budget: b}}}, nil
	case authmanifest.KindServiceAccount:
		policy, err := policyOf(d)
		if err != nil {
			return Document{}, err
		}
		return Document{ServiceAccount: &ServiceAccountDTO{APIVersion: d.APIVersion, Kind: d.Kind, Metadata: wm,
			Spec: ServiceAccountSpec{Project: d.ServiceAccount.Project, Policy: policy, Enabled: d.ServiceAccount.Enabled}}}, nil
	case authmanifest.KindGroup:
		return Document{Group: &GroupDTO{APIVersion: d.APIVersion, Kind: d.Kind, Metadata: wm,
			Spec: GroupSpec{Members: d.Group.Members, Enabled: d.Group.Enabled}}}, nil
	case authmanifest.KindRole:
		return Document{Role: &RoleDTO{APIVersion: d.APIVersion, Kind: d.Kind, Metadata: wm, Spec: roleSpecOf(d.Role)}}, nil
	case authmanifest.KindRoleBinding:
		return Document{RoleBinding: &RoleBindingDTO{APIVersion: d.APIVersion, Kind: d.Kind, Metadata: wm, Spec: roleBindingSpecOf(d.RoleBinding)}}, nil
	case authmanifest.KindUser:
		return Document{User: d}, nil
	}
	return Document{}, fmt.Errorf("unknown kind %q", d.Kind)
}

func roleSpecOf(s *authmanifest.RoleSpec) RoleSpec {
	rules := make([]RoleRuleDTO, 0, len(s.Rules))
	for _, r := range s.Rules {
		rules = append(rules, RoleRuleDTO{Kinds: r.Kinds, Verbs: r.Verbs})
	}
	return RoleSpec{Rules: rules, Enabled: s.Enabled}
}

func roleBindingSpecOf(s *authmanifest.RoleBindingSpec) RoleBindingSpec {
	subjects := make([]SubjectDTO, 0, len(s.Subjects))
	for _, sub := range s.Subjects {
		subjects = append(subjects, SubjectDTO{Kind: sub.Kind, Name: sub.Name})
	}
	return RoleBindingSpec{
		Role:     s.Role,
		Scope:    WireOwner{Kind: meta.OwnerKind(s.Scope.Kind), Name: s.Scope.Name, ID: s.Scope.ID},
		Subjects: subjects, Enabled: s.Enabled,
	}
}

func teamDoc(d TeamDTO) *authmanifest.Document {
	doc := authDoc(d.APIVersion, authmanifest.KindTeam, d.Metadata)
	doc.Team = &authmanifest.TeamSpec{Enabled: d.Spec.Enabled}
	setField(doc, "budget", d.Spec.Budget, d.Spec.Budget != nil)
	return doc
}

func projectDoc(d ProjectDTO) *authmanifest.Document {
	doc := authDoc(d.APIVersion, authmanifest.KindProject, d.Metadata)
	doc.Project = &authmanifest.ProjectSpec{Team: d.Spec.Team, Enabled: d.Spec.Enabled}
	setField(doc, "budget", d.Spec.Budget, d.Spec.Budget != nil)
	return doc
}

func serviceAccountDoc(d ServiceAccountDTO) *authmanifest.Document {
	doc := authDoc(d.APIVersion, authmanifest.KindServiceAccount, d.Metadata)
	doc.ServiceAccount = &authmanifest.ServiceAccountSpec{Project: d.Spec.Project, Enabled: d.Spec.Enabled}
	setField(doc, "policy", d.Spec.Policy, d.Spec.Policy != "")
	return doc
}

func groupDoc(d GroupDTO) *authmanifest.Document {
	doc := authDoc(d.APIVersion, authmanifest.KindGroup, d.Metadata)
	doc.Group = &authmanifest.GroupSpec{Members: d.Spec.Members, Enabled: d.Spec.Enabled}
	return doc
}

func roleDoc(d RoleDTO) *authmanifest.Document {
	doc := authDoc(d.APIVersion, authmanifest.KindRole, d.Metadata)
	rules := make([]authmanifest.Rule, 0, len(d.Spec.Rules))
	for _, r := range d.Spec.Rules {
		rules = append(rules, authmanifest.Rule{Kinds: r.Kinds, Verbs: r.Verbs})
	}
	doc.Role = &authmanifest.RoleSpec{Rules: rules, Enabled: d.Spec.Enabled}
	return doc
}

func roleBindingDoc(d RoleBindingDTO) *authmanifest.Document {
	doc := authDoc(d.APIVersion, authmanifest.KindRoleBinding, d.Metadata)
	subjects := make([]authmanifest.Subject, 0, len(d.Spec.Subjects))
	for _, s := range d.Spec.Subjects {
		subjects = append(subjects, authmanifest.Subject{Kind: s.Kind, Name: s.Name})
	}
	doc.RoleBinding = &authmanifest.RoleBindingSpec{
		Role:     d.Spec.Role,
		Scope:    authmanifest.Scope{Kind: string(d.Spec.Scope.Kind), Name: d.Spec.Scope.Name, ID: d.Spec.Scope.ID},
		Subjects: subjects, Enabled: d.Spec.Enabled,
	}
	return doc
}

// rendered drops the error of a module From call: it fails only when a product field's FromAttribute does, and relay's never do.
func rendered(d *authmanifest.Document, _ error) *authmanifest.Document { return d }
