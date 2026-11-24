package catalog

import (
	"slices"
	"sort"

	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/meta"
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
	return s.serviceAccountsByOwner[meta.Owner{Kind: meta.OwnerProject, ID: projectID}]
}

// ServiceAccountsForTeam returns the accounts the team owns itself, not
// those of its projects, sorted by name. The returned slice must not be
// mutated.
func (s *Snapshot) ServiceAccountsForTeam(teamID string) []*serviceaccount.ServiceAccount {
	return s.serviceAccountsByOwner[meta.Owner{Kind: meta.OwnerTeam, ID: teamID}]
}

// ServiceAccountsForUser returns the accounts the user owns, their personal
// bots, sorted by name. The returned slice must not be mutated.
func (s *Snapshot) ServiceAccountsForUser(userID string) []*serviceaccount.ServiceAccount {
	return s.serviceAccountsByOwner[meta.Owner{Kind: meta.OwnerUser, ID: userID}]
}

// ServiceAccountIDsOwnedBy returns the ids of the service accounts a team or a user owns, disabled ones included, sorted: whose traffic the owner's usage readers read. The returned slice must not be mutated.
func (s *Snapshot) ServiceAccountIDsOwnedBy(owner meta.Owner) []string {
	return s.serviceAccountIDsByOwner[owner]
}

// indexServiceAccountOwner records sa under its owner when a team or a user owns it, whether or not it is enabled.
func (s *Snapshot) indexServiceAccountOwner(sa *serviceaccount.ServiceAccount) {
	owner, id := sa.Meta.Owner, sa.Meta.ID
	if owner.Kind != meta.OwnerTeam && owner.Kind != meta.OwnerUser {
		return
	}
	if prev, ok := s.serviceAccountOwners[id]; ok {
		if prev == owner {
			return
		}
		s.dropServiceAccountOwner(id)
	}
	if s.serviceAccountOwners == nil {
		s.serviceAccountIDsByOwner, s.serviceAccountOwners = map[meta.Owner][]string{}, map[string]meta.Owner{}
	}
	have := s.serviceAccountIDsByOwner[owner]
	at, _ := slices.BinarySearch(have, id)
	s.serviceAccountIDsByOwner[owner] = slices.Insert(slices.Clone(have), at, id)
	s.serviceAccountOwners[id] = owner
}

// dropServiceAccountOwner removes a deleted service account from its owner's ids.
func (s *Snapshot) dropServiceAccountOwner(id string) {
	owner, ok := s.serviceAccountOwners[id]
	if !ok {
		return
	}
	delete(s.serviceAccountOwners, id)
	rest := slices.DeleteFunc(slices.Clone(s.serviceAccountIDsByOwner[owner]), func(have string) bool { return have == id })
	if len(rest) == 0 {
		delete(s.serviceAccountIDsByOwner, owner)
		return
	}
	s.serviceAccountIDsByOwner[owner] = rest
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
