package directory_test

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/directory/memstore"
	"github.com/wyolet/relay/auth/rbac"
)

var errStore = errors.New("store down")

// flaky fails reads, writes or both of the store it wraps.
type flaky[T any] struct {
	directory.Store[T]
	reads, writes *bool
}

func (f flaky[T]) Get(ctx context.Context, id string) (*T, error) {
	if *f.reads {
		return nil, errStore
	}
	return f.Store.Get(ctx, id)
}

func (f flaky[T]) List(ctx context.Context) ([]*T, error) {
	if *f.reads {
		return nil, errStore
	}
	return f.Store.List(ctx)
}

func (f flaky[T]) Upsert(ctx context.Context, row *T) error {
	if *f.writes {
		return errStore
	}
	return f.Store.Upsert(ctx, row)
}

func (f flaky[T]) Delete(ctx context.Context, id string) error {
	if *f.writes {
		return errStore
	}
	return f.Store.Delete(ctx, id)
}

type flakyUsers struct {
	directory.UserStore
	reads, writes *bool
}

func (f flakyUsers) Get(ctx context.Context, id string) (*directory.User, error) {
	if *f.reads {
		return nil, errStore
	}
	return f.UserStore.Get(ctx, id)
}

func (f flakyUsers) ByUsername(ctx context.Context, n string) (*directory.User, error) {
	if *f.reads {
		return nil, errStore
	}
	return f.UserStore.ByUsername(ctx, n)
}

func (f flakyUsers) ByIdentity(ctx context.Context, id directory.Identity) (*directory.User, error) {
	if *f.reads {
		return nil, errStore
	}
	return f.UserStore.ByIdentity(ctx, id)
}

func (f flakyUsers) Upsert(ctx context.Context, u *directory.User) error {
	if *f.writes {
		return errStore
	}
	return f.UserStore.Upsert(ctx, u)
}

func (f flakyUsers) SetDisabled(ctx context.Context, id string, disabled bool) (bool, error) {
	if *f.writes {
		return false, errStore
	}
	return f.UserStore.SetDisabled(ctx, id, disabled)
}

func (f flakyUsers) UpdateProvisioned(ctx context.Context, source, id string, in directory.ProvisionedUser) error {
	if *f.writes {
		return errStore
	}
	return f.UserStore.UpdateProvisioned(ctx, source, id, in)
}

func (f flakyUsers) Delete(ctx context.Context, id string) error {
	if *f.writes {
		return errStore
	}
	return f.UserStore.Delete(ctx, id)
}

func (f flakyUsers) BumpTokenVersion(ctx context.Context, id string) error {
	if *f.writes {
		return errStore
	}
	return f.UserStore.BumpTokenVersion(ctx, id)
}

// TestStoreErrorsReachTheCaller runs every operation against stores that fail reads, then writes, and requires the store's error back, never a success or a different refusal.
func TestStoreErrorsReachTheCaller(t *testing.T) {
	var reads, writes bool
	m := memstore.New().Stores()
	s := directory.Stores{
		Users:           flakyUsers{m.Users, &reads, &writes},
		Groups:          flaky[directory.Group]{m.Groups, &reads, &writes},
		Teams:           flaky[directory.Team]{m.Teams, &reads, &writes},
		Projects:        flaky[directory.Project]{m.Projects, &reads, &writes},
		ServiceAccounts: flaky[directory.ServiceAccount]{m.ServiceAccounts, &reads, &writes},
		Roles:           flaky[directory.Role]{m.Roles, &reads, &writes},
		RoleBindings:    flaky[directory.RoleBinding]{m.RoleBindings, &reads, &writes},
	}
	d, err := directory.New(directory.Config{Stores: s, Grants: &grants{}})
	must(t, err)

	u := &directory.User{Username: "ada"}
	must(t, d.CreateUser(ctx, nil, u))
	team := &directory.Team{Meta: directory.Metadata{Name: "core"}}
	must(t, d.CreateTeam(ctx, team))
	p := &directory.Project{Meta: directory.Metadata{Name: "api"}, Spec: directory.ProjectSpec{TeamID: team.Meta.ID}}
	must(t, d.CreateProject(ctx, p))
	sa := &directory.ServiceAccount{Meta: directory.Metadata{Name: "ci"}, Spec: directory.ServiceAccountSpec{ProjectID: p.Meta.ID}}
	must(t, d.CreateServiceAccount(ctx, sa))
	g := &directory.Group{Meta: directory.Metadata{Name: "ops"}}
	must(t, d.CreateGroup(ctx, admin, g))
	r := &directory.Role{Meta: directory.Metadata{Name: "reader"}, Spec: directory.RoleSpec{Rules: []rbac.Rule{{Kinds: []string{"d"}, Verbs: []string{"get"}}}}}
	must(t, d.CreateRole(ctx, r))
	b := &directory.RoleBinding{Meta: directory.Metadata{Name: "readers"}, Spec: directory.RoleBindingSpec{RoleID: r.Meta.ID,
		Scope: directory.Global, Subjects: []directory.Subject{{Kind: directory.SubjectUser, ID: u.ID}, {Kind: directory.SubjectServiceAccount, ID: sa.Meta.ID}}}}
	must(t, d.CreateRoleBinding(ctx, admin, b))
	pb := &directory.RoleBinding{Meta: directory.Metadata{ID: b.Meta.ID, Name: "readers"}, Spec: directory.RoleBindingSpec{RoleID: r.Meta.ID,
		Scope: directory.Global, Subjects: []directory.Subject{directory.AllAuthenticated}}}

	idp := directory.Identity{Issuer: "https://idp.example.com", Subject: "s"}
	ops := map[string]func() error{
		"create user":       func() error { return d.CreateUser(ctx, nil, &directory.User{Username: "x", Identity: idp}) },
		"create user by id": func() error { return d.CreateUser(ctx, nil, &directory.User{ID: "x", Username: "x"}) },
		"create user roles": func() error {
			return d.CreateUser(ctx, admin, &directory.User{Username: "x", Roles: []string{"reader"}})
		},
		"update user": func() error {
			return d.UpdateUser(ctx, admin, &directory.User{ID: u.ID, Username: "ada", Identity: idp, Roles: []string{"reader"}})
		},
		"disable user": func() error { return d.DisableUser(ctx, u.ID) },
		"delete user":  func() error { return d.DeleteUser(ctx, u.ID) },
		"create group": func() error {
			return d.CreateGroup(ctx, admin, &directory.Group{Meta: directory.Metadata{DisplayName: "x"}})
		},
		"create group id": func() error {
			return d.CreateGroup(ctx, admin, &directory.Group{Meta: directory.Metadata{ID: "x", Name: "x"}})
		},
		"update group": func() error {
			return d.UpdateGroup(ctx, admin, &directory.Group{Meta: directory.Metadata{ID: g.Meta.ID}, Spec: directory.GroupSpec{MemberIDs: []string{u.ID}}})
		},
		"add member":   func() error { return d.AddGroupMember(ctx, admin, g.Meta.ID, u.ID) },
		"delete group": func() error { return d.DeleteGroup(ctx, g.Meta.ID) },
		"create team":  func() error { return d.CreateTeam(ctx, &directory.Team{Meta: directory.Metadata{DisplayName: "x"}}) },
		"update team":  func() error { return d.UpdateTeam(ctx, &directory.Team{Meta: directory.Metadata{ID: team.Meta.ID}}) },
		"create project": func() error {
			return d.CreateProject(ctx, &directory.Project{Meta: directory.Metadata{Name: "x"}, Spec: p.Spec})
		},
		"update project": func() error {
			return d.UpdateProject(ctx, &directory.Project{Meta: directory.Metadata{ID: p.Meta.ID}, Spec: p.Spec})
		},
		"delete project": func() error { return d.DeleteProject(ctx, p.Meta.ID) },
		"create account": func() error {
			return d.CreateServiceAccount(ctx, &directory.ServiceAccount{Meta: directory.Metadata{Name: "x"}, Spec: sa.Spec})
		},
		"update account": func() error {
			return d.UpdateServiceAccount(ctx, &directory.ServiceAccount{Meta: directory.Metadata{ID: sa.Meta.ID}, Spec: sa.Spec})
		},
		"create role": func() error {
			return d.CreateRole(ctx, &directory.Role{Meta: directory.Metadata{DisplayName: "x"}, Spec: r.Spec})
		},
		"update role": func() error {
			return d.UpdateRole(ctx, admin, &directory.Role{Meta: directory.Metadata{ID: r.Meta.ID}, Spec: r.Spec})
		},
		"create binding": func() error {
			return d.CreateRoleBinding(ctx, admin, &directory.RoleBinding{Meta: directory.Metadata{Name: "x"}, Spec: b.Spec})
		},
		"update binding":  func() error { return d.UpdateRoleBinding(ctx, admin, pb) },
		"bind at a team":  func() error { return d.UpdateRoleBinding(ctx, admin, scoped(pb, directory.OwnerTeam, team.Meta.ID)) },
		"bind at project": func() error { return d.UpdateRoleBinding(ctx, admin, scoped(pb, directory.OwnerProject, p.Meta.ID)) },
		"delete binding":  func() error { return d.DeleteRoleBinding(ctx, b.Meta.ID) },
	}
	for _, mode := range []struct {
		name          string
		reads, writes bool
	}{{"reads", true, false}, {"writes", false, true}} {
		reads, writes = mode.reads, mode.writes
		for name, op := range ops {
			if err := op(); !errors.Is(err, errStore) {
				t.Errorf("%s with failing %s: err = %v, want the store's", name, mode.name, err)
			}
		}
	}
}

func scoped(b *directory.RoleBinding, kind, id string) *directory.RoleBinding {
	c := *b
	c.Spec.Scope = rbac.Scope{Kind: kind, ID: id}
	return &c
}
