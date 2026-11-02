package control

import (
	"context"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/team"
)

// MyContext is one scope the caller may create rows in.
type MyContext struct {
	Kind        string `json:"kind"             enum:"personal,team,project" doc:"personal is the caller's own scope, where rows with an omitted owner land; team and project name a tenancy row."`
	ID          string `json:"id"               doc:"The caller's user id for personal, else the team or project id. The owner a row created in this context takes: {kind: user, id} for personal, {kind: team|project, id} otherwise."`
	Name        string `json:"name"             doc:"The caller's username for personal, else the team or project slug."`
	DisplayName string `json:"displayName"      doc:"The team or project display name; the username for personal."`
	TeamID      string `json:"teamId,omitempty" doc:"The project's team. Projects only."`
	Admin       bool   `json:"admin"            doc:"The caller holds every permission of the scope's built-in admin role there (team-admin for a team, project-admin for a project), through any binding: direct, a group's, or one on the project's team. Always true for personal."`
}

type myContextsOut struct {
	Body []MyContext
}

// contextKinds are the kinds a context's rows are created as; a scope where the caller may create none of them is no context of theirs.
var contextKinds = []string{"policy", "pool", "account", "host-key", "budget", "rate-limit", "policy-binding", "key", "service-account", "role-binding"}

func registerMyContexts(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "list_my_contexts",
		Method:      http.MethodGet,
		Path:        "/me/contexts",
		Summary:     "List the scopes the caller may create rows in",
		Description: "The contexts the caller may act in: their personal scope first, then each team, then each project, each list by slug, where the caller may create at least one policy, pool, account, host key, budget, rate limit, policy binding, key, service account or role binding. Grants through groups and through a team-level role count, so a team admin is listed with the team's projects. The admin token is no user: it gets every team and project with admin true and no personal scope. Read from the in-memory catalog, so a write shows within about a second.",
		Tags:        []string{"accounts"},
		Middlewares: protect,
		Errors:      []int{401, 503},
	}, func(ctx context.Context, _ *struct{}) (*myContextsOut, error) {
		a := actor.From(ctx)
		if !a.IsAuthenticated() {
			return nil, mapAuthzErr(authz.ErrUnauthenticated)
		}
		if d.Catalog == nil || d.Catalog.Current() == nil {
			return nil, huma.Error503ServiceUnavailable("the catalog is not loaded yet")
		}
		// Unaudited: the probes are reads of what the caller may do, not decisions on this request.
		return &myContextsOut{Body: myContexts(ctx, unaudited(d.Authz), d.Catalog.Current(), a)}, nil
	})
}

// contextSource is the slice of the catalog snapshot the contexts read.
type contextSource interface {
	AllTeams() []*team.Team
	AllProjects() []*project.Project
	RoleByName(name string) (*role.Role, bool)
}

func myContexts(ctx context.Context, az authz.Authorizer, src contextSource, a *actor.Actor) []MyContext {
	out := []MyContext{}
	if !a.AdminToken && a.UserID != "" {
		out = append(out, MyContext{Kind: "personal", ID: a.UserID, Name: a.Username, DisplayName: a.Username, Admin: true})
	}
	teamAdmin, _ := src.RoleByName(role.BuiltinTeamAdmin)
	for _, t := range src.AllTeams() {
		scope := meta.Owner{Kind: meta.OwnerTeam, ID: t.Meta.ID}
		if c, ok := scopeContext(ctx, az, scope, teamAdmin); ok {
			c.Name, c.DisplayName = t.Meta.Name, label(t.Meta)
			out = append(out, c)
		}
	}
	projectAdmin, _ := src.RoleByName(role.BuiltinProjectAdmin)
	for _, p := range src.AllProjects() {
		scope := meta.Owner{Kind: meta.OwnerProject, ID: p.Meta.ID}
		if c, ok := scopeContext(ctx, az, scope, projectAdmin); ok {
			c.Name, c.DisplayName, c.TeamID = p.Meta.Name, label(p.Meta), p.Spec.TeamID
			out = append(out, c)
		}
	}
	return out
}

// scopeContext is scope as a context of the caller, false when they may create nothing there. Admin is the grant check binding adminRole there would pass, so it agrees with what the caller could hand out.
func scopeContext(ctx context.Context, az authz.Authorizer, scope meta.Owner, adminRole *role.Role) (MyContext, bool) {
	c := MyContext{Kind: string(scope.Kind), ID: scope.ID}
	if adminRole != nil {
		c.Admin = authz.CheckGrant(ctx, az, adminRole, scope) == nil
	} else {
		c.Admin = authz.IsAdmin(ctx)
	}
	if c.Admin {
		return c, true
	}
	ok := slices.ContainsFunc(contextKinds, func(kind string) bool {
		return az.Authorize(ctx, authz.Plural(kind)+".create", authz.Resource{Kind: kind, Owner: &scope}) == nil
	})
	return c, ok
}
