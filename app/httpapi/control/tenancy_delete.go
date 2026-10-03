package control

import (
	"context"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/team"
)

// guardTeamDelete refuses deleting a team that still has projects: the
// foreign keys would take them, their service accounts and their keys along.
func guardTeamDelete(d Deps) mutationGuard[team.Team] {
	return func(ctx context.Context, action string, existing, _ *team.Team) error {
		if action != "delete" || existing == nil || d.Stores == nil || d.Stores.Project == nil {
			return nil
		}
		projects, err := d.Stores.Project.List(ctx)
		if err != nil {
			return huma.Error500InternalServerError("list projects: " + err.Error())
		}
		return dependentsConflict("team", existing.Meta.Name, project.OfTeam(existing.Meta.ID, projects, nil))
	}
}

// refuseProjectWithRows refuses deleting a project that still has rows under
// it (see project.Rows).
func refuseProjectWithRows(ctx context.Context, d Deps, existing *project.Project) error {
	if existing == nil || d.Stores == nil {
		return nil
	}
	s := d.Stores
	var rows project.Rows
	var err error
	if rows.ServiceAccounts, err = s.ServiceAccount.List(ctx); err != nil {
		return huma.Error500InternalServerError("list service accounts: " + err.Error())
	}
	if rows.Keys, err = s.Key.List(ctx); err != nil {
		return huma.Error500InternalServerError("list keys: " + err.Error())
	}
	if rows.Policies, err = s.Policy.List(ctx); err != nil {
		return huma.Error500InternalServerError("list policies: " + err.Error())
	}
	if rows.HostKeys, err = s.HostKey.List(ctx); err != nil {
		return huma.Error500InternalServerError("list host keys: " + err.Error())
	}
	if rows.RateLimits, err = s.RateLimit.List(ctx); err != nil {
		return huma.Error500InternalServerError("list rate limits: " + err.Error())
	}
	if rows.PolicyBindings, err = s.PolicyBinding.List(ctx); err != nil {
		return huma.Error500InternalServerError("list policy bindings: " + err.Error())
	}
	return dependentsConflict("project", existing.Meta.Name, rows.Dependents(existing.Meta.ID, nil))
}

func dependentsConflict(kind, name string, deps []string) error {
	if len(deps) == 0 {
		return nil
	}
	return huma.Error409Conflict(fmt.Sprintf("%s %q %s", kind, name, &project.DependentsError{Rows: deps}))
}
