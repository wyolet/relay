package manifest

import (
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
)

// ---------------------------------------------------------------------------
// Team / Project
// ---------------------------------------------------------------------------

// resolveScopeOwner rewrites a team-, project-, user-, or host-kind owner
// from the wire name to its id. Rows in other scopes are untouched.
func resolveScopeOwner(o *meta.Owner, idx Resolver) {
	if o.ID == "" {
		return
	}
	switch o.Kind {
	case meta.OwnerTeam:
		if id, ok := idx.TeamID(o.ID); ok {
			o.ID = id
		}
	case meta.OwnerProject:
		if id, ok := idx.ProjectID(o.ID); ok {
			o.ID = id
		}
	case meta.OwnerUser:
		if id, ok := idx.UserID(o.ID); ok {
			o.ID = id
		}
	case meta.OwnerHost:
		if id, ok := idx.HostID(o.ID); ok {
			o.ID = id
		}
	}
}

func toBudget(b *BudgetDTO) *team.Budget {
	if b == nil {
		return nil
	}
	out := &team.Budget{Amount: b.Amount, Period: b.Period}
	out.Default()
	return out
}

func fromBudget(b *team.Budget) *BudgetDTO {
	if b == nil {
		return nil
	}
	return &BudgetDTO{Amount: b.Amount, Period: b.Period}
}

// ToTeam translates through the auth module, which makes every team system-owned: a user-owned one would let its author inherit every binding made at its scope.
func ToTeam(d TeamDTO, idx Resolver) (*team.Team, error) {
	row, err := AuthManifests.ToTeam(teamDoc(d), authRefs{idx})
	if err != nil {
		return nil, fromAuthError(err)
	}
	return team.FromDirectory(row)
}

// FromTeam renders through the auth module; the metadata keeps relay's wire form.
func FromTeam(t *team.Team, rev ReverseResolver) TeamDTO {
	// A budget of plain strings always marshals.
	row, _ := t.ToDirectory()
	d, _ := fromAuthDoc(rendered(AuthManifests.FromTeam(row, authNames{rev})))
	dto := *d.Team
	dto.APIVersion, dto.Metadata = APIVersion, metaToWire(t.Meta)
	return dto
}

// ToProject resolves the owning team name → id. Owner mirrors spec.team,
// so it is re-derived rather than read from the wire form.
func ToProject(d ProjectDTO, idx Resolver) (*project.Project, error) {
	row, err := AuthManifests.ToProject(projectDoc(d), authRefs{idx})
	if err != nil {
		return nil, fromAuthError(err)
	}
	return project.FromDirectory(row)
}

func FromProject(p *project.Project, rev ReverseResolver) ProjectDTO {
	row, _ := p.ToDirectory()
	d, _ := fromAuthDoc(rendered(AuthManifests.FromProject(row, authNames{rev})))
	dto := *d.Project
	dto.APIVersion, dto.Metadata = APIVersion, metaToWire(p.Meta)
	dto.Metadata.Owner.Name = dto.Spec.Team
	return dto
}

// ---------------------------------------------------------------------------
// ServiceAccount / Group
// ---------------------------------------------------------------------------

// ToServiceAccount resolves the owning project name → id and the optional
// policy override. Owner mirrors spec.project, so it is re-derived rather
// than read from the wire form.
func ToServiceAccount(d ServiceAccountDTO, idx Resolver) (*serviceaccount.ServiceAccount, error) {
	row, err := AuthManifests.ToServiceAccount(serviceAccountDoc(d), authRefs{idx})
	if err != nil {
		return nil, fromAuthError(err)
	}
	return serviceaccount.FromDirectory(row)
}

func FromServiceAccount(sa *serviceaccount.ServiceAccount, rev ReverseResolver) ServiceAccountDTO {
	row, _ := sa.ToDirectory()
	d, _ := fromAuthDoc(rendered(AuthManifests.FromServiceAccount(row, authNames{rev})))
	dto := *d.ServiceAccount
	dto.APIVersion, dto.Metadata = APIVersion, metaToWire(sa.Meta)
	dto.Metadata.Owner.Name = dto.Spec.Project
	return dto
}

// ToGroup resolves member usernames → user ids. The module makes every group system-owned: a user-owned one named after an IdP group would inherit that group's bindings.
func ToGroup(d GroupDTO, idx Resolver) (*group.Group, error) {
	row, err := AuthManifests.ToGroup(groupDoc(d), authRefs{idx})
	if err != nil {
		return nil, fromAuthError(err)
	}
	return group.FromDirectory(row), nil
}

func FromGroup(g *group.Group, rev ReverseResolver) GroupDTO {
	d, _ := fromAuthDoc(rendered(AuthManifests.FromGroup(g.ToDirectory(), authNames{rev})))
	dto := *d.Group
	dto.APIVersion, dto.Metadata = APIVersion, metaToWire(g.Meta)
	return dto
}
