package catalog

import (
	"sort"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
)

// ── ServiceAccount ────────────────────────────────────────────────────────

func (c *Catalog) ApplyServiceAccountUpsert(sa *serviceaccount.ServiceAccount) error {
	if !sa.IsEnabled() {
		return c.ApplyServiceAccountDelete(sa.Meta.ID)
	}
	if err := sa.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if handled, err := c.recoverAbsentLocked(refServiceAccount, sa.Meta.ID); handled {
		return err
	}
	s := c.snap.Load().clone()
	clean, keep := sanitizeServiceAccount(sa, snapIDs(s.projectsByID), s.policyResolvable)
	if !keep {
		deleteServiceAccount(s, sa.Meta.ID)
		c.snap.Store(s)
		return nil
	}
	insertServiceAccount(s, clean)
	reindexKeySubjects(s, key.PrincipalServiceAccount, clean.Meta.ID)
	c.snap.Store(s)
	return nil
}

func (c *Catalog) ApplyServiceAccountDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deleteServiceAccount(s, id)
	c.snap.Store(s)
	return nil
}

func insertServiceAccount(s *Snapshot, sa *serviceaccount.ServiceAccount) {
	if old, ok := s.serviceAccountsByID[sa.Meta.ID]; ok {
		s.unregisterRefs(refKey{Kind: refServiceAccount, ID: old.Meta.ID}, outboundServiceAccountRefs(old))
		delete(s.serviceAccountsByName, old.Meta.Name)
		delete(s.serviceAccountsByID, old.Meta.ID)
		removeServiceAccountFromProject(s, old)
	}
	s.serviceAccountsByID[sa.Meta.ID] = sa
	s.serviceAccountsByName[sa.Meta.Name] = sa
	insertServiceAccountIntoProject(s, sa)
	s.registerRefs(refKey{Kind: refServiceAccount, ID: sa.Meta.ID}, outboundServiceAccountRefs(sa))
}

func deleteServiceAccount(s *Snapshot, id string) {
	sa, ok := s.serviceAccountsByID[id]
	if !ok {
		return
	}
	s.unregisterRefs(refKey{Kind: refServiceAccount, ID: id}, outboundServiceAccountRefs(sa))
	delete(s.serviceAccountsByID, id)
	delete(s.serviceAccountsByName, sa.Meta.Name)
	removeServiceAccountFromProject(s, sa)
	cascadeDelete(s, refServiceAccount, id)
}

// insertServiceAccountIntoProject keeps serviceAccountsByProject sorted by
// account name, the order build() produces.
func insertServiceAccountIntoProject(s *Snapshot, sa *serviceaccount.ServiceAccount) {
	list := append(s.serviceAccountsByProject[sa.Spec.ProjectID], sa)
	sort.Slice(list, func(i, j int) bool { return list[i].Meta.Name < list[j].Meta.Name })
	s.serviceAccountsByProject[sa.Spec.ProjectID] = list
}

func removeServiceAccountFromProject(s *Snapshot, sa *serviceaccount.ServiceAccount) {
	list := s.serviceAccountsByProject[sa.Spec.ProjectID]
	out := make([]*serviceaccount.ServiceAccount, 0, len(list))
	for _, cur := range list {
		if cur.Meta.ID != sa.Meta.ID {
			out = append(out, cur)
		}
	}
	if len(out) == 0 {
		delete(s.serviceAccountsByProject, sa.Spec.ProjectID)
		return
	}
	s.serviceAccountsByProject[sa.Spec.ProjectID] = out
}

// ── Team ──────────────────────────────────────────────────────────────────

func (c *Catalog) ApplyTeamUpsert(t *team.Team) error {
	if !t.IsEnabled() {
		return c.ApplyTeamDelete(t.Meta.ID)
	}
	if err := t.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if handled, err := c.recoverAbsentLocked(refTeam, t.Meta.ID); handled {
		return err
	}
	s := c.snap.Load().clone()
	insertTeam(s, t)
	c.snap.Store(s)
	return nil
}

func (c *Catalog) ApplyTeamDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deleteTeam(s, id)
	c.snap.Store(s)
	return nil
}

func insertTeam(s *Snapshot, t *team.Team) {
	if old, ok := s.teamsByID[t.Meta.ID]; ok {
		delete(s.teamsByName, old.Meta.Name)
		delete(s.teamsByID, old.Meta.ID)
	}
	s.teamsByID[t.Meta.ID] = t
	s.teamsByName[t.Meta.Name] = t
}

func deleteTeam(s *Snapshot, id string) {
	t, ok := s.teamsByID[id]
	if !ok {
		return
	}
	delete(s.teamsByID, id)
	delete(s.teamsByName, t.Meta.Name)
	cascadeDelete(s, refTeam, id)
}

// ── Project ───────────────────────────────────────────────────────────────

func (c *Catalog) ApplyProjectUpsert(p *project.Project) error {
	if !p.IsEnabled() {
		return c.ApplyProjectDelete(p.Meta.ID)
	}
	if err := p.Validate(); err != nil {
		return err
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if handled, err := c.recoverAbsentLocked(refProject, p.Meta.ID); handled {
		return err
	}
	s := c.snap.Load().clone()
	clean, keep := sanitizeProject(p, snapIDs(s.teamsByID))
	if !keep {
		deleteProject(s, p.Meta.ID)
		c.snap.Store(s)
		return nil
	}
	// A project's slug is part of its service accounts' subjects, so a
	// rename reaches every key under it — but only a rename does. Any other
	// project edit leaves every subject list unchanged.
	old, existed := s.projectsByID[clean.Meta.ID]
	insertProject(s, clean)
	if !existed || old.Meta.Name != clean.Meta.Name {
		reindexAllKeySubjects(s)
	}
	c.snap.Store(s)
	return nil
}

func (c *Catalog) ApplyProjectDelete(id string) error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	s := c.snap.Load().clone()
	deleteProject(s, id)
	reindexAllKeySubjects(s)
	c.snap.Store(s)
	return nil
}

func insertProject(s *Snapshot, p *project.Project) {
	if old, ok := s.projectsByID[p.Meta.ID]; ok {
		s.unregisterRefs(refKey{Kind: refProject, ID: old.Meta.ID}, outboundProjectRefs(old))
		delete(s.projectsByName, old.Meta.Name)
		delete(s.projectsByID, old.Meta.ID)
		removeProjectFromTeam(s, old)
	}
	s.projectsByID[p.Meta.ID] = p
	s.projectsByName[p.Meta.Name] = p
	insertProjectIntoTeam(s, p)
	s.registerRefs(refKey{Kind: refProject, ID: p.Meta.ID}, outboundProjectRefs(p))
}

func deleteProject(s *Snapshot, id string) {
	p, ok := s.projectsByID[id]
	if !ok {
		return
	}
	s.unregisterRefs(refKey{Kind: refProject, ID: id}, outboundProjectRefs(p))
	delete(s.projectsByID, id)
	delete(s.projectsByName, p.Meta.Name)
	removeProjectFromTeam(s, p)
	cascadeDelete(s, refProject, id)
}

// insertProjectIntoTeam keeps projectsByTeam sorted by project name, the
// order build() produces.
func insertProjectIntoTeam(s *Snapshot, p *project.Project) {
	list := append(s.projectsByTeam[p.Spec.TeamID], p)
	sort.Slice(list, func(i, j int) bool { return list[i].Meta.Name < list[j].Meta.Name })
	s.projectsByTeam[p.Spec.TeamID] = list
}

func removeProjectFromTeam(s *Snapshot, p *project.Project) {
	list := s.projectsByTeam[p.Spec.TeamID]
	out := make([]*project.Project, 0, len(list))
	for _, cur := range list {
		if cur.Meta.ID != p.Meta.ID {
			out = append(out, cur)
		}
	}
	if len(out) == 0 {
		delete(s.projectsByTeam, p.Spec.TeamID)
		return
	}
	s.projectsByTeam[p.Spec.TeamID] = out
}
