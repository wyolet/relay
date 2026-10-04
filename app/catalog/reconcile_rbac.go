package catalog

import (
	"sort"

	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
)

// ── Group ─────────────────────────────────────────────────────────────────

func (c *Catalog) ApplyGroupUpsert(g *group.Group) error {
	if !g.IsEnabled() {
		return c.ApplyGroupDelete(g.Meta.ID)
	}
	if err := g.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	old, hadOld := s.groupsByID[g.Meta.ID]
	insertGroup(s, g)
	if hadOld {
		reindexMemberSubjects(s, old.Spec.MemberIDs)
	}
	reindexMemberSubjects(s, g.Spec.MemberIDs)
	c.snap.Store(s)
	return nil
}

func (c *Catalog) ApplyGroupDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	members := []string(nil)
	if g, ok := s.groupsByID[id]; ok {
		members = g.Spec.MemberIDs
	}
	deleteGroup(s, id)
	reindexMemberSubjects(s, members)
	c.snap.Store(s)
	return nil
}

func insertGroup(s *Snapshot, g *group.Group) {
	if old, ok := s.groupsByID[g.Meta.ID]; ok {
		delete(s.groupsByName, old.Meta.Name)
		delete(s.groupsByID, old.Meta.ID)
		removeGroupFromUsers(s, old)
	}
	s.groupsByID[g.Meta.ID] = g
	s.groupsByName[g.Meta.Name] = g
	for _, uid := range g.Spec.MemberIDs {
		list := append(s.groupsByUser[uid], g.Meta.Name)
		sort.Strings(list)
		s.groupsByUser[uid] = list
	}
}

func deleteGroup(s *Snapshot, id string) {
	g, ok := s.groupsByID[id]
	if !ok {
		return
	}
	delete(s.groupsByID, id)
	delete(s.groupsByName, g.Meta.Name)
	removeGroupFromUsers(s, g)
}

func removeGroupFromUsers(s *Snapshot, g *group.Group) {
	for _, uid := range g.Spec.MemberIDs {
		list := s.groupsByUser[uid]
		out := make([]string, 0, len(list))
		for _, name := range list {
			if name != g.Meta.Name {
				out = append(out, name)
			}
		}
		if len(out) == 0 {
			delete(s.groupsByUser, uid)
			continue
		}
		s.groupsByUser[uid] = out
	}
}

// ── Role ──────────────────────────────────────────────────────────────────

func (c *Catalog) ApplyRoleUpsert(r *role.Role) error {
	if !r.IsEnabled() {
		return c.ApplyRoleDelete(r.Meta.ID)
	}
	if err := r.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if handled, err := c.recoverAbsentLocked(refRole, r.Meta.ID); handled {
		return err
	}
	s := c.snap.Load().clone()
	insertRole(s, r)
	c.snap.Store(s)
	return nil
}

func (c *Catalog) ApplyRoleDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deleteRole(s, id)
	c.snap.Store(s)
	return nil
}

func insertRole(s *Snapshot, r *role.Role) {
	if old, ok := s.rolesByID[r.Meta.ID]; ok {
		delete(s.rolesByName, old.Meta.Name)
		delete(s.rolesByID, old.Meta.ID)
	}
	s.rolesByID[r.Meta.ID] = r
	s.rolesByName[r.Meta.Name] = r
}

func deleteRole(s *Snapshot, id string) {
	r, ok := s.rolesByID[id]
	if !ok {
		return
	}
	delete(s.rolesByID, id)
	delete(s.rolesByName, r.Meta.Name)
	cascadeDelete(s, refRole, id)
}

// ── RoleBinding ───────────────────────────────────────────────────────────

func (c *Catalog) ApplyRoleBindingUpsert(b *rolebinding.RoleBinding) error {
	if !b.IsEnabled() || len(b.Spec.Subjects) == 0 {
		// PG cascading away the last subject leaves a binding that grants
		// nothing and fails Validate; treat it as a delete so the stale
		// grant leaves the snapshot.
		return c.ApplyRoleBindingDelete(b.Meta.ID)
	}
	if err := b.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	clean, keep := sanitizeRoleBinding(b, snapIDs(s.rolesByID), snapIDs(s.teamsByID), snapIDs(s.projectsByID))
	if !keep {
		deleteRoleBinding(s, b.Meta.ID)
		c.snap.Store(s)
		return nil
	}
	insertRoleBinding(s, clean)
	c.snap.Store(s)
	return nil
}

func (c *Catalog) ApplyRoleBindingDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deleteRoleBinding(s, id)
	c.snap.Store(s)
	return nil
}

func insertRoleBinding(s *Snapshot, b *rolebinding.RoleBinding) {
	if old, ok := s.roleBindingsByID[b.Meta.ID]; ok {
		s.unregisterRefs(refKey{Kind: refRoleBinding, ID: old.Meta.ID}, outboundRoleBindingRefs(old))
		delete(s.roleBindingsByID, old.Meta.ID)
		removeRoleBindingFromSubjects(s, old)
	}
	s.roleBindingsByID[b.Meta.ID] = b
	for i := range b.Spec.Subjects {
		key := b.Spec.Subjects[i].Key()
		list := append(s.roleBindingsBySubject[key], b)
		sortRoleBindings(list)
		s.roleBindingsBySubject[key] = list
	}
	s.registerRefs(refKey{Kind: refRoleBinding, ID: b.Meta.ID}, outboundRoleBindingRefs(b))
}

func deleteRoleBinding(s *Snapshot, id string) {
	b, ok := s.roleBindingsByID[id]
	if !ok {
		return
	}
	s.unregisterRefs(refKey{Kind: refRoleBinding, ID: id}, outboundRoleBindingRefs(b))
	delete(s.roleBindingsByID, id)
	removeRoleBindingFromSubjects(s, b)
}

func removeRoleBindingFromSubjects(s *Snapshot, b *rolebinding.RoleBinding) {
	for i := range b.Spec.Subjects {
		key := b.Spec.Subjects[i].Key()
		list := s.roleBindingsBySubject[key]
		out := make([]*rolebinding.RoleBinding, 0, len(list))
		for _, cur := range list {
			if cur.Meta.ID != b.Meta.ID {
				out = append(out, cur)
			}
		}
		if len(out) == 0 {
			delete(s.roleBindingsBySubject, key)
			continue
		}
		s.roleBindingsBySubject[key] = out
	}
}

// ── PolicyBinding ─────────────────────────────────────────────────────────

// A policy binding names a project, subjects and a policy — none of which
// the allowed-combo sets are a function of — so these two publish directly
// instead of rebuilding every policy's grants.
func (c *Catalog) ApplyPolicyBindingUpsert(b *policybinding.PolicyBinding) error {
	if !b.IsEnabled() || len(b.Spec.Subjects) == 0 {
		// Same rule as ApplyRoleBindingUpsert: no subjects, no binding.
		return c.ApplyPolicyBindingDelete(b.Meta.ID)
	}
	if err := b.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	clean, keep := sanitizePolicyBinding(b, snapIDs(s.projectsByID), s.policyResolvable)
	if !keep {
		deletePolicyBinding(s, b.Meta.ID)
		c.snap.Store(s)
		return nil
	}
	insertPolicyBinding(s, clean)
	c.snap.Store(s)
	return nil
}

func (c *Catalog) ApplyPolicyBindingDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deletePolicyBinding(s, id)
	c.snap.Store(s)
	return nil
}

func insertPolicyBinding(s *Snapshot, b *policybinding.PolicyBinding) {
	if old, ok := s.policyBindingsByID[b.Meta.ID]; ok {
		s.unregisterRefs(refKey{Kind: refPolicyBinding, ID: old.Meta.ID}, outboundPolicyBindingRefs(old))
		delete(s.policyBindingsByID, old.Meta.ID)
		removePolicyBindingFromProject(s, old)
	}
	s.policyBindingsByID[b.Meta.ID] = b
	list := append(s.policyBindingsByProject[b.Spec.ProjectID], b)
	sortPolicyBindings(list)
	s.policyBindingsByProject[b.Spec.ProjectID] = list
	s.registerRefs(refKey{Kind: refPolicyBinding, ID: b.Meta.ID}, outboundPolicyBindingRefs(b))
}

func deletePolicyBinding(s *Snapshot, id string) {
	b, ok := s.policyBindingsByID[id]
	if !ok {
		return
	}
	s.unregisterRefs(refKey{Kind: refPolicyBinding, ID: id}, outboundPolicyBindingRefs(b))
	delete(s.policyBindingsByID, id)
	removePolicyBindingFromProject(s, b)
}

func removePolicyBindingFromProject(s *Snapshot, b *policybinding.PolicyBinding) {
	list := s.policyBindingsByProject[b.Spec.ProjectID]
	out := make([]*policybinding.PolicyBinding, 0, len(list))
	for _, cur := range list {
		if cur.Meta.ID != b.Meta.ID {
			out = append(out, cur)
		}
	}
	if len(out) == 0 {
		delete(s.policyBindingsByProject, b.Spec.ProjectID)
		return
	}
	s.policyBindingsByProject[b.Spec.ProjectID] = out
}
