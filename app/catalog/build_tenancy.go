package catalog

import (
	"sort"

	"github.com/wyolet/relay/app/project"
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
	if _, ok := teams[p.Spec.TeamID]; !ok {
		return nil, false
	}
	clean := *p
	return &clean, true
}
