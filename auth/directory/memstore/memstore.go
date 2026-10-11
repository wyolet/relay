// Package memstore is an in-memory directory.Stores. It behaves as auth/store/postgres does, resource versions, unique names, references and delete cascades included, so code over the directory can be tested without a database.
//
// Out of scope: durability and scale. Every row lives in maps behind one mutex and every read copies; it is meant for tests and examples, not production.
package memstore

import (
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/wyolet/relay/auth/directory"
)

// Store holds every row. The zero value is not usable; call New.
type Store struct {
	mu sync.Mutex

	users     map[string]*directory.User
	userOrder []string

	groups          *table[directory.Group]
	teams           *table[directory.Team]
	projects        *table[directory.Project]
	serviceAccounts *table[directory.ServiceAccount]
	roles           *table[directory.Role]
	bindings        *table[directory.RoleBinding]
}

// New returns an empty store.
func New() *Store {
	s := &Store{users: map[string]*directory.User{}}
	s.groups = newTable(func(g *directory.Group) *directory.Metadata { return &g.Meta }, cloneGroup)
	s.teams = newTable(func(t *directory.Team) *directory.Metadata { return &t.Meta }, cloneTeam)
	s.projects = newTable(func(p *directory.Project) *directory.Metadata { return &p.Meta }, cloneProject)
	s.serviceAccounts = newTable(func(sa *directory.ServiceAccount) *directory.Metadata { return &sa.Meta }, cloneServiceAccount)
	s.roles = newTable(func(r *directory.Role) *directory.Metadata { return &r.Meta }, cloneRole)
	s.bindings = newTable(func(b *directory.RoleBinding) *directory.Metadata { return &b.Meta }, cloneBinding)
	return s
}

// Stores returns the store as the directory's interfaces.
func (s *Store) Stores() directory.Stores {
	return directory.Stores{
		Users:  userStore{s},
		Groups: kindStore[directory.Group]{s: s, t: s.groups, check: s.checkGroup},
		Teams:  kindStore[directory.Team]{s: s, t: s.teams, onDelete: s.cascadeTeam},
		Projects: kindStore[directory.Project]{s: s, t: s.projects, check: s.checkProject, onDelete: s.cascadeProject,
			prepare: (*directory.Project).StampOwner},
		ServiceAccounts: kindStore[directory.ServiceAccount]{s: s, t: s.serviceAccounts, check: s.checkServiceAccount, onDelete: s.dropServiceAccount,
			prepare: (*directory.ServiceAccount).StampOwner},
		Roles: kindStore[directory.Role]{s: s, t: s.roles, onDelete: s.cascadeRole},
		RoleBindings: kindStore[directory.RoleBinding]{s: s, t: s.bindings, check: s.checkBinding,
			prepare: (*directory.RoleBinding).StampOwner},
	}
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{directory.ErrInvalid}, args...)...)
}

// The check functions stand in for the foreign keys; they run with s.mu held.

func (s *Store) checkGroup(g *directory.Group) error {
	for _, id := range g.Spec.MemberIDs {
		if _, ok := s.users[id]; !ok {
			return invalid("group %q: user %q does not exist", g.Meta.Name, id)
		}
	}
	if g.Spec.External.ID == "" {
		return nil
	}
	for id, other := range s.groups.rows {
		if id != g.Meta.ID && other.Spec.External == g.Spec.External {
			return fmt.Errorf("%w: group %q: external id is taken", directory.ErrConflict, g.Meta.Name)
		}
	}
	return nil
}

func (s *Store) checkProject(p *directory.Project) error {
	if _, ok := s.teams.rows[p.Spec.TeamID]; !ok {
		return invalid("project %q: team %q does not exist", p.Meta.Name, p.Spec.TeamID)
	}
	return nil
}

func (s *Store) checkServiceAccount(sa *directory.ServiceAccount) error {
	if _, ok := s.projects.rows[sa.Spec.ProjectID]; !ok {
		return invalid("service account %q: project %q does not exist", sa.Meta.Name, sa.Spec.ProjectID)
	}
	return nil
}

func (s *Store) checkBinding(b *directory.RoleBinding) error {
	if _, ok := s.roles.rows[b.Spec.RoleID]; !ok {
		return invalid("role binding %q: role %q does not exist", b.Meta.Name, b.Spec.RoleID)
	}
	for _, sub := range b.Spec.Subjects {
		missing := false
		switch sub.Kind {
		case directory.SubjectUser:
			_, ok := s.users[sub.ID]
			missing = !ok
		case directory.SubjectServiceAccount:
			_, ok := s.serviceAccounts.rows[sub.ID]
			missing = !ok
		}
		if missing {
			return invalid("role binding %q: subject %q does not exist", b.Meta.Name, sub.Key())
		}
	}
	return nil
}

// The cascade functions stand in for ON DELETE CASCADE; they run with s.mu held.

func (s *Store) cascadeTeam(id string) {
	for pid, p := range s.projects.rows {
		if p.Spec.TeamID == id {
			delete(s.projects.rows, pid)
			s.cascadeProject(pid)
		}
	}
}

func (s *Store) cascadeProject(id string) {
	for said, sa := range s.serviceAccounts.rows {
		if sa.Spec.ProjectID == id {
			delete(s.serviceAccounts.rows, said)
			s.dropServiceAccount(said)
		}
	}
}

func (s *Store) cascadeRole(id string) {
	for bid, b := range s.bindings.rows {
		if b.Spec.RoleID == id {
			delete(s.bindings.rows, bid)
		}
	}
}

func (s *Store) dropServiceAccount(id string) { s.dropSubject(directory.SubjectServiceAccount, id) }

func (s *Store) dropUser(id string) {
	for _, g := range s.groups.rows {
		g.Spec.MemberIDs = slices.DeleteFunc(g.Spec.MemberIDs, func(m string) bool { return m == id })
		if len(g.Spec.MemberIDs) == 0 {
			g.Spec.MemberIDs = nil
		}
	}
	s.dropSubject(directory.SubjectUser, id)
}

// dropSubject removes the subject from every binding and leaves the binding in place, as the junction's foreign key does.
func (s *Store) dropSubject(kind directory.SubjectKind, id string) {
	for _, b := range s.bindings.rows {
		b.Spec.Subjects = slices.DeleteFunc(b.Spec.Subjects, func(sub directory.Subject) bool {
			return sub.Kind == kind && sub.ID == id
		})
		if len(b.Spec.Subjects) == 0 {
			b.Spec.Subjects = nil
		}
	}
}

func now() time.Time { return time.Now().UTC() }
