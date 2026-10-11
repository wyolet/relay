package directory

import (
	"context"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/rbac"
)

// Hierarchy answers the two lookups a scope chain needs. A product with the hierarchy in memory implements it over that; StoreHierarchy reads through the stores.
type Hierarchy interface {
	// ProjectTeam returns the team a live project is in; found is false for a missing project.
	ProjectTeam(ctx context.Context, projectID string) (teamID string, found bool, err error)
	// TeamExists reports whether the team is live.
	TeamExists(ctx context.Context, teamID string) (bool, error)
}

// ScopeResolver is the default rbac.ScopeResolver: a resource's owner, then the owner's team when the owner is a project, then Global. A team or project resource is its own innermost scope, since its owner names the scope outside it. An owner of any other kind, or a project or team that is not live, collapses to Global, which leaves the resource to global bindings. A product with scopes of its own wraps it.
type ScopeResolver struct {
	Hierarchy Hierarchy
}

// ChainFor returns res's scopes, most specific first, ending in Global.
func (r ScopeResolver) ChainFor(ctx context.Context, res authz.Resource) ([]rbac.Scope, error) {
	if res.ID != "" {
		switch res.Kind {
		case OwnerTeam:
			return []rbac.Scope{{Kind: OwnerTeam, ID: res.ID}, Global}, nil
		case OwnerProject:
			team, found, err := r.Hierarchy.ProjectTeam(ctx, res.ID)
			if err != nil {
				return nil, err
			}
			if found {
				return []rbac.Scope{{Kind: OwnerProject, ID: res.ID}, {Kind: OwnerTeam, ID: team}, Global}, nil
			}
			// A project being created, or one no longer live: its owner names the team the write is authorized at.
		}
	}
	return r.ownerChain(ctx, res.Owner)
}

func (r ScopeResolver) ownerChain(ctx context.Context, o *authz.Owner) ([]rbac.Scope, error) {
	if o == nil {
		return []rbac.Scope{Global}, nil
	}
	switch o.Kind {
	case OwnerProject:
		team, found, err := r.Hierarchy.ProjectTeam(ctx, o.ID)
		if err != nil {
			return nil, err
		}
		if found {
			return []rbac.Scope{{Kind: OwnerProject, ID: o.ID}, {Kind: OwnerTeam, ID: team}, Global}, nil
		}
	case OwnerTeam:
		ok, err := r.Hierarchy.TeamExists(ctx, o.ID)
		if err != nil {
			return nil, err
		}
		if ok {
			return []rbac.Scope{{Kind: OwnerTeam, ID: o.ID}, Global}, nil
		}
	}
	return []rbac.Scope{Global}, nil
}

// StoreHierarchy reads the hierarchy through the stores, one Get per lookup. A disabled team or project counts as not live, and so does a project whose team is not, so a disabled tenancy row grants nothing below it.
func StoreHierarchy(s Stores) Hierarchy { return storeHierarchy{s} }

type storeHierarchy struct{ s Stores }

func (h storeHierarchy) ProjectTeam(ctx context.Context, projectID string) (string, bool, error) {
	p, err := h.s.Projects.Get(ctx, projectID)
	if err != nil || p == nil || !p.IsEnabled() {
		return "", false, err
	}
	ok, err := h.TeamExists(ctx, p.Spec.TeamID)
	if err != nil || !ok {
		return "", false, err
	}
	return p.Spec.TeamID, true, nil
}

func (h storeHierarchy) TeamExists(ctx context.Context, teamID string) (bool, error) {
	t, err := h.s.Teams.Get(ctx, teamID)
	if err != nil || t == nil {
		return false, err
	}
	return t.IsEnabled(), nil
}
