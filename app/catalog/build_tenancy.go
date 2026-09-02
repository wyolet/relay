package catalog

import (
	"sort"

	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
)

// addTeams folds Teams into the snapshot. Teams have no outbound refs, so
// there is nothing to sanitize.
func (s *Snapshot) addTeams(teams []*team.Team) {
	for _, t := range teams {
		s.teamsByID[t.Meta.ID] = t
		s.teamsByName[t.Meta.Name] = t
	}
}

// addProjects folds Projects into the snapshot. A project whose Team is
// missing/disabled is dropped — with no team there is no scope to serve it
// under.
//
// Indexes: projectsByID, projectsByName, and projectsByTeam (sorted by
// project name, for per-team enumeration).
func (s *Snapshot) addProjects(projects []*project.Project, teams idSet) {
	for _, p := range projects {
		clean, keep := sanitizeProject(p, teams)
		if !keep {
			continue
		}
		s.projectsByID[clean.Meta.ID] = clean
		s.projectsByName[clean.Meta.Name] = clean
		s.projectsByTeam[clean.Spec.TeamID] = append(s.projectsByTeam[clean.Spec.TeamID], clean)
		s.registerRefs(refKey{Kind: refProject, ID: clean.Meta.ID}, outboundProjectRefs(clean))
	}
	for teamID := range s.projectsByTeam {
		list := s.projectsByTeam[teamID]
		sort.Slice(list, func(i, j int) bool { return list[i].Meta.Name < list[j].Meta.Name })
	}
}

// sanitizeProject drops the project when its Team is missing.
func sanitizeProject(p *project.Project, teams idSet) (*project.Project, bool) {
	if !teams(p.Spec.TeamID) {
		return nil, false
	}
	clean := *p
	return &clean, true
}

// addServiceAccounts folds ServiceAccounts into the snapshot.
//
// Indexes: serviceAccountsByID, serviceAccountsByName, and
// serviceAccountsByProject (sorted by account name).
func (s *Snapshot) addServiceAccounts(sas []*serviceaccount.ServiceAccount, projects, policies idSet) {
	for _, sa := range sas {
		clean, keep := sanitizeServiceAccount(sa, projects, policies)
		if !keep {
			continue
		}
		s.serviceAccountsByID[clean.Meta.ID] = clean
		s.serviceAccountsByName[clean.Meta.Name] = clean
		s.serviceAccountsByProject[clean.Spec.ProjectID] = append(s.serviceAccountsByProject[clean.Spec.ProjectID], clean)
		s.registerRefs(refKey{Kind: refServiceAccount, ID: clean.Meta.ID}, outboundServiceAccountRefs(clean))
	}
	for projectID := range s.serviceAccountsByProject {
		list := s.serviceAccountsByProject[projectID]
		sort.Slice(list, func(i, j int) bool { return list[i].Meta.Name < list[j].Meta.Name })
	}
}

// sanitizeServiceAccount drops the account when its Project is missing, or
// when a set policy override does not resolve — a dangling override would
// silently fall through to a broader grant.
func sanitizeServiceAccount(sa *serviceaccount.ServiceAccount, projects, policies idSet) (*serviceaccount.ServiceAccount, bool) {
	if !projects(sa.Spec.ProjectID) {
		return nil, false
	}
	if sa.Spec.PolicyID != "" {
		if !policies(sa.Spec.PolicyID) {
			return nil, false
		}
	}
	clean := *sa
	return &clean, true
}

// addGroups folds Groups into the snapshot. Groups have no outbound refs
// (a member id that is not a user is inert, not invalid), so there is
// nothing to sanitize.
//
// Indexes: groupsByID, groupsByName, and groupsByUser (user id → sorted
// group names).
func (s *Snapshot) addGroups(groups []*group.Group) {
	for _, g := range groups {
		s.groupsByID[g.Meta.ID] = g
		s.groupsByName[g.Meta.Name] = g
		for _, uid := range g.Spec.MemberIDs {
			s.groupsByUser[uid] = append(s.groupsByUser[uid], g.Meta.Name)
		}
	}
	for uid := range s.groupsByUser {
		sort.Strings(s.groupsByUser[uid])
	}
}
