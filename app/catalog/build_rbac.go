package catalog

import (
	"sort"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
)

// addRoles folds Roles into the snapshot. Roles have no outbound refs, so
// there is nothing to sanitize.
func (s *Snapshot) addRoles(roles []*role.Role) {
	for _, r := range roles {
		s.rolesByID[r.Meta.ID] = r
		s.rolesByName[r.Meta.Name] = r
	}
}

// addRoleBindings folds RoleBindings into the snapshot.
//
// Indexes: roleBindingsByID and roleBindingsBySubject (subject key → the
// bindings naming it, sorted by binding name).
func (s *Snapshot) addRoleBindings(bindings []*rolebinding.RoleBinding, roles, teams, projects idSet) {
	for _, b := range bindings {
		clean, keep := sanitizeRoleBinding(b, roles, teams, projects)
		if !keep {
			continue
		}
		s.roleBindingsByID[clean.Meta.ID] = clean
		for i := range clean.Spec.Subjects {
			key := clean.Spec.Subjects[i].Key()
			s.roleBindingsBySubject[key] = append(s.roleBindingsBySubject[key], clean)
		}
		s.registerRefs(refKey{Kind: refRoleBinding, ID: clean.Meta.ID}, outboundRoleBindingRefs(clean))
	}
	for key := range s.roleBindingsBySubject {
		sortRoleBindings(s.roleBindingsBySubject[key])
	}
}

// sanitizeRoleBinding drops the binding when its Role is missing or its
// scope names a team/project that is not in the snapshot — with no scope
// target there is nothing for the grant to apply to.
func sanitizeRoleBinding(b *rolebinding.RoleBinding, roles, teams, projects idSet) (*rolebinding.RoleBinding, bool) {
	if !roles(b.Spec.RoleID) {
		return nil, false
	}
	switch b.Spec.Scope.Kind {
	case meta.OwnerTeam:
		if !teams(b.Spec.Scope.ID) {
			return nil, false
		}
	case meta.OwnerProject:
		if !projects(b.Spec.Scope.ID) {
			return nil, false
		}
	}
	clean := *b
	return &clean, true
}

// addPolicyBindings folds PolicyBindings into the snapshot.
//
// Indexes: policyBindingsByID and policyBindingsByProject (sorted by
// effective priority, then name — the order resolution reads them in).
func (s *Snapshot) addPolicyBindings(bindings []*policybinding.PolicyBinding, projects, policies idSet) {
	for _, b := range bindings {
		clean, keep := sanitizePolicyBinding(b, projects, policies)
		if !keep {
			continue
		}
		s.policyBindingsByID[clean.Meta.ID] = clean
		s.policyBindingsByProject[clean.Spec.ProjectID] = append(s.policyBindingsByProject[clean.Spec.ProjectID], clean)
		s.registerRefs(refKey{Kind: refPolicyBinding, ID: clean.Meta.ID}, outboundPolicyBindingRefs(clean))
	}
	for projectID := range s.policyBindingsByProject {
		sortPolicyBindings(s.policyBindingsByProject[projectID])
	}
}

// sanitizePolicyBinding drops the binding when its Project or its Policy is
// missing: both edges are hard, a dangling one would resolve a principal to
// nothing.
func sanitizePolicyBinding(b *policybinding.PolicyBinding, projects, policies idSet) (*policybinding.PolicyBinding, bool) {
	if !projects(b.Spec.ProjectID) {
		return nil, false
	}
	if !policies(b.Spec.PolicyID) {
		return nil, false
	}
	clean := *b
	clean.IndexSubjects()
	return &clean, true
}

func sortRoleBindings(list []*rolebinding.RoleBinding) {
	sort.Slice(list, func(i, j int) bool { return list[i].Meta.Name < list[j].Meta.Name })
}

func sortPolicyBindings(list []*policybinding.PolicyBinding) {
	sort.Slice(list, func(i, j int) bool {
		pi, pj := list[i].EffectivePriority(), list[j].EffectivePriority()
		if pi != pj {
			return pi < pj
		}
		return list[i].Meta.Name < list[j].Meta.Name
	})
}
