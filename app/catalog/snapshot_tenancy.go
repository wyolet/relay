package catalog

import (
	"sort"

	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
)

// Team returns the enabled Team with this id, or false.
func (s *Snapshot) Team(id string) (*team.Team, bool) {
	t, ok := s.teamsByID[id]
	return t, ok
}

// TeamByName returns the enabled Team with this slug, or false.
func (s *Snapshot) TeamByName(name string) (*team.Team, bool) {
	t, ok := s.teamsByName[name]
	return t, ok
}

// Project returns the enabled Project with this id, or false.
func (s *Snapshot) Project(id string) (*project.Project, bool) {
	p, ok := s.projectsByID[id]
	return p, ok
}

// ProjectByName returns the enabled Project with this slug, or false.
func (s *Snapshot) ProjectByName(name string) (*project.Project, bool) {
	p, ok := s.projectsByName[name]
	return p, ok
}

// ProjectsInTeam returns the team's projects, sorted by project name. The
// returned slice must not be mutated.
func (s *Snapshot) ProjectsInTeam(teamID string) []*project.Project {
	return s.projectsByTeam[teamID]
}

// AllTeams returns every Team in the snapshot, sorted by slug.
func (s *Snapshot) AllTeams() []*team.Team {
	out := make([]*team.Team, 0, len(s.teamsByID))
	for _, t := range s.teamsByID {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}

// AllProjects returns every Project in the snapshot, sorted by slug.
func (s *Snapshot) AllProjects() []*project.Project {
	out := make([]*project.Project, 0, len(s.projectsByID))
	for _, p := range s.projectsByID {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}

// ServiceAccount returns the enabled ServiceAccount with this id, or false.
func (s *Snapshot) ServiceAccount(id string) (*serviceaccount.ServiceAccount, bool) {
	sa, ok := s.serviceAccountsByID[id]
	return sa, ok
}

// ServiceAccountByName returns the enabled ServiceAccount with this slug, or false.
func (s *Snapshot) ServiceAccountByName(name string) (*serviceaccount.ServiceAccount, bool) {
	sa, ok := s.serviceAccountsByName[name]
	return sa, ok
}

// ServiceAccountsForProject returns the project's accounts, sorted by name.
// The returned slice must not be mutated.
func (s *Snapshot) ServiceAccountsForProject(projectID string) []*serviceaccount.ServiceAccount {
	return s.serviceAccountsByProject[projectID]
}

// AllServiceAccounts returns every ServiceAccount in the snapshot, sorted by slug.
func (s *Snapshot) AllServiceAccounts() []*serviceaccount.ServiceAccount {
	out := make([]*serviceaccount.ServiceAccount, 0, len(s.serviceAccountsByID))
	for _, sa := range s.serviceAccountsByID {
		out = append(out, sa)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}
