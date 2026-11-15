package catalog

import (
	"sort"

	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/pkg/meta"
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
// target there is nothing for the grant to apply to. A system scope is
// indexed as the global one it names.
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
	clean.StampOwner()
	return &clean, true
}

// addPolicyBindings folds PolicyBindings into the snapshot.
//
// Indexes: policyBindingsByID and policyBindingsByProject (sorted by
// effective priority, then name — the order resolution reads them in).
func (s *Snapshot) addPolicyBindings(bindings []*policybinding.PolicyBinding, scopes func(meta.Owner) bool, policies idSet) {
	for _, b := range bindings {
		clean, keep := sanitizePolicyBinding(b, scopes, policies)
		if !keep {
			continue
		}
		s.policyBindingsByID[clean.Meta.ID] = clean
		if pid := owningProject(clean); pid != "" {
			s.policyBindingsByProject[pid] = append(s.policyBindingsByProject[pid], clean)
		}
		s.registerRefs(refKey{Kind: refPolicyBinding, ID: clean.Meta.ID}, outboundPolicyBindingRefs(clean))
	}
	for projectID := range s.policyBindingsByProject {
		sortPolicyBindings(s.policyBindingsByProject[projectID])
	}
}

// ownerScopeExists reports whether a policy binding's owner scope is in the snapshot: its project, its team, or the organization. Users are not snapshot rows; a user-owned binding goes with its user through the foreign key.
func (s *Snapshot) ownerScopeExists(o meta.Owner) bool {
	switch o.Kind {
	case meta.OwnerProject:
		_, ok := s.projectsByID[o.ID]
		return ok
	case meta.OwnerTeam:
		_, ok := s.teamsByID[o.ID]
		return ok
	case meta.OwnerUser:
		return o.ID != ""
	}
	return o == meta.Global
}

// sanitizePolicyBinding drops the binding when its owner scope or its Policy
// is missing: both edges are hard, a dangling one would resolve a principal
// to nothing.
func sanitizePolicyBinding(b *policybinding.PolicyBinding, scopes func(meta.Owner) bool, policies idSet) (*policybinding.PolicyBinding, bool) {
	clean := *b
	clean.Meta.Owner = clean.Meta.Owner.AsGlobalScope()
	if !scopes(clean.Meta.Owner) {
		return nil, false
	}
	if !policies(clean.Spec.PolicyID) {
		return nil, false
	}
	return &clean, true
}

// owningProject is the id of the project owning b, "" when a team, a user or the system does.
func owningProject(b *policybinding.PolicyBinding) string {
	if b.Meta.Owner.Kind == meta.OwnerProject {
		return b.Meta.Owner.ID
	}
	return ""
}

func sortRoleBindings(list []*rolebinding.RoleBinding) {
	sort.Slice(list, func(i, j int) bool { return list[i].Meta.Name < list[j].Meta.Name })
}

func sortPolicyBindings(list []*policybinding.PolicyBinding) {
	sort.Slice(list, func(i, j int) bool { return list[i].Meta.Name < list[j].Meta.Name })
}
