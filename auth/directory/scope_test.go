package directory_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/directory/memstore"
	"github.com/wyolet/relay/auth/rbac"
)

var _ rbac.ScopeResolver = directory.ScopeResolver{}

func TestScopeResolver(t *testing.T) {
	s := memstore.New().Stores()
	no := false
	team := &directory.Team{Meta: directory.Metadata{ID: "t-1", Name: "core", Owner: authz.Owner{Kind: "system"}}}
	off := &directory.Team{Meta: directory.Metadata{ID: "t-off", Name: "off", Owner: authz.Owner{Kind: "system"}}, Spec: directory.TeamSpec{Enabled: &no}}
	p := &directory.Project{Meta: directory.Metadata{ID: "p-1", Name: "api"}, Spec: directory.ProjectSpec{TeamID: "t-1"}}
	pOff := &directory.Project{Meta: directory.Metadata{ID: "p-off", Name: "off"}, Spec: directory.ProjectSpec{TeamID: "t-1", Enabled: &no}}
	pInOff := &directory.Project{Meta: directory.Metadata{ID: "p-in-off", Name: "in-off"}, Spec: directory.ProjectSpec{TeamID: "t-off"}}
	for _, err := range []error{
		s.Teams.Upsert(ctx, team), s.Teams.Upsert(ctx, off),
		s.Projects.Upsert(ctx, p), s.Projects.Upsert(ctx, pOff), s.Projects.Upsert(ctx, pInOff),
	} {
		must(t, err)
	}
	r := directory.ScopeResolver{Hierarchy: directory.StoreHierarchy(s)}

	teamScope := rbac.Scope{Kind: "team", ID: "t-1"}
	projectScope := rbac.Scope{Kind: "project", ID: "p-1"}
	g := directory.Global
	owner := func(kind, id string) *authz.Owner { return &authz.Owner{Kind: kind, ID: id} }
	tests := []struct {
		name string
		res  authz.Resource
		want []rbac.Scope
	}{
		{"no owner", authz.Resource{Kind: "documents"}, []rbac.Scope{g}},
		{"owned by a project", authz.Resource{Kind: "documents", ID: "d", Owner: owner("project", "p-1")}, []rbac.Scope{projectScope, teamScope, g}},
		{"owned by a team", authz.Resource{Kind: "documents", Owner: owner("team", "t-1")}, []rbac.Scope{teamScope, g}},
		{"owned by a user", authz.Resource{Kind: "documents", Owner: owner("user", "u-1")}, []rbac.Scope{g}},
		{"owned by the system", authz.Resource{Kind: "documents", Owner: owner("system", "")}, []rbac.Scope{g}},
		{"owned by a missing project", authz.Resource{Kind: "documents", Owner: owner("project", "nope")}, []rbac.Scope{g}},
		{"owned by a disabled project", authz.Resource{Kind: "documents", Owner: owner("project", "p-off")}, []rbac.Scope{g}},
		{"owned by a project in a disabled team", authz.Resource{Kind: "documents", Owner: owner("project", "p-in-off")}, []rbac.Scope{g}},
		{"owned by a disabled team", authz.Resource{Kind: "documents", Owner: owner("team", "t-off")}, []rbac.Scope{g}},
		{"owned by a missing team", authz.Resource{Kind: "documents", Owner: owner("team", "nope")}, []rbac.Scope{g}},
		{"a team is its own scope", authz.Resource{Kind: "team", ID: "t-1", Owner: owner("system", "")}, []rbac.Scope{teamScope, g}},
		{"a project is its own scope", authz.Resource{Kind: "project", ID: "p-1", Owner: owner("team", "t-1")}, []rbac.Scope{projectScope, teamScope, g}},
		{"a project being created", authz.Resource{Kind: "project", ID: "new", Owner: owner("team", "t-1")}, []rbac.Scope{teamScope, g}},
		{"a project list", authz.Resource{Kind: "project"}, []rbac.Scope{g}},
		{"the grant check's probe", authz.Resource{Kind: "documents", Owner: owner("project", "p-1")}, []rbac.Scope{projectScope, teamScope, g}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.ChainFor(ctx, tt.res)
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ChainFor = %v, %v; want %v", got, err, tt.want)
			}
		})
	}

	down := directory.ScopeResolver{Hierarchy: failing{}}
	for _, res := range []authz.Resource{
		{Kind: "project", ID: "p-1"},
		{Kind: "documents", Owner: owner("project", "p-1")},
		{Kind: "documents", Owner: owner("team", "t-1")},
	} {
		if _, err := down.ChainFor(ctx, res); !errors.Is(err, errDown) {
			t.Fatalf("ChainFor(%+v) with a failing lookup = %v, want it returned", res, err)
		}
	}
}

var errDown = errors.New("down")

type failing struct{}

func (failing) ProjectTeam(context.Context, string) (string, bool, error) { return "", false, errDown }
func (failing) TeamExists(context.Context, string) (bool, error)          { return false, errDown }
