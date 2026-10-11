package authz

import (
	"context"
	"slices"

	"github.com/wyolet/relay/app/meta"
	coreauthz "github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/rbac"
)

// Relay's shortcuts ahead of the role bindings, listed here in the order the evaluation runs them. Each also answers ScopeOf with the rows it admits. The admin actor is the engine's Admin flag, checked before all of them.

const (
	ownerUser     = string(meta.OwnerUser)
	ownerSystem   = string(meta.OwnerSystem)
	ownerHost     = string(meta.OwnerHost)
	ownerProvider = string(meta.OwnerProvider)
	ownerTeam     = string(meta.OwnerTeam)
)

func readVerb(verb string) bool { return verb == "get" || verb == "list" }

func decision(allow bool) rbac.Decision {
	if allow {
		return rbac.Allow
	}
	return rbac.Continue
}

// personalRowRule: a personal row's owner holds every verb on it, except on shared catalog kinds, which no user may own.
type personalRowRule struct{}

func (personalRowRule) Decide(_ context.Context, p *rbac.Principal, kind, _ string, res coreauthz.Resource) (rbac.Decision, error) {
	o := res.Owner
	return decision(o != nil && o.Kind == ownerUser && o.ID != "" && o.ID == p.ID && !sharedCatalogKinds[kind]), nil
}

func (personalRowRule) Filter(_ context.Context, p *rbac.Principal, kind, _ string) (rbac.Filter, error) {
	if p.ID == "" || sharedCatalogKinds[kind] {
		return rbac.Filter{}, nil
	}
	return rbac.Filter{Owners: []coreauthz.Owner{{Kind: ownerUser, ID: p.ID}}}, nil
}

// catalogReadRule: catalog rows read for every authenticated caller.
type catalogReadRule struct{}

func (catalogReadRule) Decide(_ context.Context, _ *rbac.Principal, kind, verb string, res coreauthz.Resource) (rbac.Decision, error) {
	return decision(readVerb(verb) && catalogKinds[kind] && (res.Owner == nil || isCatalogOwner(res.Owner.Kind))), nil
}

func (catalogReadRule) Filter(_ context.Context, _ *rbac.Principal, kind, verb string) (rbac.Filter, error) {
	if !readVerb(verb) || !catalogKinds[kind] {
		return rbac.Filter{}, nil
	}
	return rbac.Filter{OwnerKinds: []string{ownerSystem, ownerProvider, ownerHost}}, nil
}

func isCatalogOwner(kind string) bool {
	return kind == ownerSystem || kind == ownerProvider || kind == ownerHost
}

// sharedPolicyReadRule: a policy owned by the system or by a host is shared configuration, not tenant data — the tier menu an upstream publishes and the relay-wide defaults. A scoped admin has to read them to bind or reference one.
type sharedPolicyReadRule struct{}

func (sharedPolicyReadRule) Decide(_ context.Context, _ *rbac.Principal, kind, verb string, res coreauthz.Resource) (rbac.Decision, error) {
	return decision(readVerb(verb) && kind == "policies" && res.Owner != nil &&
		(res.Owner.Kind == ownerSystem || res.Owner.Kind == ownerHost)), nil
}

func (sharedPolicyReadRule) Filter(_ context.Context, _ *rbac.Principal, kind, verb string) (rbac.Filter, error) {
	if !readVerb(verb) || kind != "policies" {
		return rbac.Filter{}, nil
	}
	return rbac.Filter{OwnerKinds: []string{ownerSystem, ownerHost}}, nil
}

// systemRoleReadRule: a system-owned Role is a shared rule set, not tenant data: creating a binding means reading the role it names, and a scoped admin holds no binding at the global scope the row lives in.
type systemRoleReadRule struct{}

func (systemRoleReadRule) Decide(_ context.Context, _ *rbac.Principal, kind, verb string, res coreauthz.Resource) (rbac.Decision, error) {
	return decision(readVerb(verb) && kind == "roles" && res.Owner != nil && res.Owner.Kind == ownerSystem), nil
}

func (systemRoleReadRule) Filter(_ context.Context, _ *rbac.Principal, kind, verb string) (rbac.Filter, error) {
	if !readVerb(verb) || kind != "roles" {
		return rbac.Filter{}, nil
	}
	return rbac.Filter{OwnerKinds: []string{ownerSystem}}, nil
}

// scopedListRule: a list call names no row and its result is filtered through Visible, so the call itself needs no binding: a deployment whose bindings have not been written yet answers an empty list rather than 403 everywhere.
type scopedListRule struct{}

func (scopedListRule) Decide(_ context.Context, _ *rbac.Principal, kind, verb string, res coreauthz.Resource) (rbac.Decision, error) {
	return decision(verb == "list" && res.Owner == nil && scopedKinds[kind]), nil
}

// Filter admits no owned row: the rule only opens the unowned list call.
func (scopedListRule) Filter(context.Context, *rbac.Principal, string, string) (rbac.Filter, error) {
	return rbac.Filter{}, nil
}

// parentTeamReadRule: working in a project includes resolving the team it belongs to, so a binding at a project reads that project's team row.
type parentTeamReadRule struct{ ev *evaluation }

func (r parentTeamReadRule) Decide(_ context.Context, p *rbac.Principal, kind, verb string, res coreauthz.Resource) (rbac.Decision, error) {
	if !readVerb(verb) || kind != "teams" || res.ID == "" {
		return rbac.Continue, nil
	}
	snap := r.ev.snapshot()
	if snap == nil {
		return rbac.Continue, nil
	}
	projects := snap.ProjectsInTeam(res.ID)
	for _, subj := range p.Subjects {
		for _, b := range snap.RoleBindingsForSubject(subj) {
			if b.Spec.Scope.Kind != meta.OwnerProject {
				continue
			}
			for _, pr := range projects {
				if pr.Meta.ID == b.Spec.Scope.ID {
					return rbac.Allow, nil
				}
			}
		}
	}
	return rbac.Continue, nil
}

// Filter lists the parent team of every project the caller is bound at. A teams row is a team row, inside its own scope, so the scope selects exactly that row.
func (r parentTeamReadRule) Filter(_ context.Context, p *rbac.Principal, kind, verb string) (rbac.Filter, error) {
	snap := r.ev.snapshot()
	if !readVerb(verb) || kind != "teams" || snap == nil {
		return rbac.Filter{}, nil
	}
	var f rbac.Filter
	for _, subj := range p.Subjects {
		for _, b := range snap.RoleBindingsForSubject(subj) {
			if b.Spec.Scope.Kind != meta.OwnerProject {
				continue
			}
			chain := snap.ScopeChainFor("project", b.Spec.Scope.ID, nil)
			if len(chain) < 2 || chain[1].Kind != meta.OwnerTeam {
				continue
			}
			team := rbac.Scope{Kind: ownerTeam, ID: chain[1].ID}
			if !slices.Contains(f.Scopes, team) {
				f.Scopes = append(f.Scopes, team)
			}
		}
	}
	return f, nil
}
