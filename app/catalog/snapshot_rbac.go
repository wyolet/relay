package catalog

import (
	"sort"

	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
)

// Group returns the enabled Group with this id, or false.
func (s *Snapshot) Group(id string) (*group.Group, bool) {
	g, ok := s.groupsByID[id]
	return g, ok
}

// GroupByName returns the enabled Group with this slug, or false.
func (s *Snapshot) GroupByName(name string) (*group.Group, bool) {
	g, ok := s.groupsByName[name]
	return g, ok
}

// GroupsForUser returns the names of the groups holding this user, sorted.
// The returned slice must not be mutated.
func (s *Snapshot) GroupsForUser(userID string) []string {
	return s.groupsByUser[userID]
}

// AllGroups returns every Group in the snapshot, sorted by slug.
func (s *Snapshot) AllGroups() []*group.Group {
	out := make([]*group.Group, 0, len(s.groupsByID))
	for _, g := range s.groupsByID {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}

// Role returns the enabled Role with this id, or false.
func (s *Snapshot) Role(id string) (*role.Role, bool) {
	r, ok := s.rolesByID[id]
	return r, ok
}

// RoleByName returns the enabled Role with this slug, or false.
func (s *Snapshot) RoleByName(name string) (*role.Role, bool) {
	r, ok := s.rolesByName[name]
	return r, ok
}

// RoleBinding returns the enabled RoleBinding with this id, or false.
func (s *Snapshot) RoleBinding(id string) (*rolebinding.RoleBinding, bool) {
	b, ok := s.roleBindingsByID[id]
	return b, ok
}

// RoleBindingsForSubject returns the bindings naming this subject
// ("user:<id>", "group:<name>", "serviceaccount:<id>"), sorted by binding
// name. The returned slice must not be mutated.
func (s *Snapshot) RoleBindingsForSubject(subject string) []*rolebinding.RoleBinding {
	return s.roleBindingsBySubject[subject]
}

// AllRoles returns every Role in the snapshot, sorted by slug.
func (s *Snapshot) AllRoles() []*role.Role {
	out := make([]*role.Role, 0, len(s.rolesByID))
	for _, r := range s.rolesByID {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}

// AllRoleBindings returns every RoleBinding in the snapshot, sorted by slug.
func (s *Snapshot) AllRoleBindings() []*rolebinding.RoleBinding {
	out := make([]*rolebinding.RoleBinding, 0, len(s.roleBindingsByID))
	for _, b := range s.roleBindingsByID {
		out = append(out, b)
	}
	sortRoleBindings(out)
	return out
}

// PolicyBinding returns the enabled PolicyBinding with this id, or false.
func (s *Snapshot) PolicyBinding(id string) (*policybinding.PolicyBinding, bool) {
	b, ok := s.policyBindingsByID[id]
	return b, ok
}

// PolicyBindingsForProject returns the project's policy bindings, ordered by
// effective priority then name. The returned slice must not be mutated.
func (s *Snapshot) PolicyBindingsForProject(projectID string) []*policybinding.PolicyBinding {
	return s.policyBindingsByProject[projectID]
}

// AllPolicyBindings returns every PolicyBinding in the snapshot, sorted by slug.
func (s *Snapshot) AllPolicyBindings() []*policybinding.PolicyBinding {
	out := make([]*policybinding.PolicyBinding, 0, len(s.policyBindingsByID))
	for _, b := range s.policyBindingsByID {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}
