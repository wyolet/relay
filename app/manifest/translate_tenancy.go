package manifest

import (
	"fmt"

	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
)

// ---------------------------------------------------------------------------
// Team / Project
// ---------------------------------------------------------------------------

// resolveScopeOwner rewrites a team-, project-, or user-kind owner from the
// wire name to its id. Rows in other scopes are untouched.
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
	}
}

func toBudget(b *BudgetDTO) *team.Budget {
	if b == nil {
		return nil
	}
	out := &team.Budget{Amount: b.Amount, Period: b.Period, OnExceed: b.OnExceed}
	out.Default()
	return out
}

func fromBudget(b *team.Budget) *BudgetDTO {
	if b == nil {
		return nil
	}
	return &BudgetDTO{Amount: b.Amount, Period: b.Period, OnExceed: b.OnExceed}
}

func ToTeam(d TeamDTO, _ Resolver) (*team.Team, error) {
	m := d.Metadata.toMeta()
	// A Team names a scope: a user-owned one would let its author inherit
	// every binding made at that scope, so the owner is not the manifest's
	// to choose.
	m.Owner = meta.Owner{Kind: meta.OwnerSystem}
	return &team.Team{
		Meta: m,
		Spec: team.Spec{
			Enabled: d.Spec.Enabled,
			Budget:  toBudget(d.Spec.Budget),
		},
	}, nil
}

func FromTeam(t *team.Team, _ ReverseResolver) TeamDTO {
	return TeamDTO{
		APIVersion: APIVersion,
		Kind:       "Team",
		Metadata:   metaToWire(t.Meta),
		Spec: TeamSpec{
			Enabled: t.Spec.Enabled,
			Budget:  fromBudget(t.Spec.Budget),
		},
	}
}

// ToProject resolves the owning team name → id. Owner mirrors spec.team,
// so it is re-derived rather than read from the wire form.
func ToProject(d ProjectDTO, idx Resolver) (*project.Project, error) {
	teamID, ok := idx.TeamID(d.Spec.Team)
	if !ok {
		return nil, fmt.Errorf("project %q: team %q not found", d.Metadata.Name, d.Spec.Team)
	}
	p := &project.Project{
		Meta: d.Metadata.toMeta(),
		Spec: project.Spec{
			TeamID:  teamID,
			Enabled: d.Spec.Enabled,
			Budget:  toBudget(d.Spec.Budget),
		},
	}
	p.StampOwner()
	return p, nil
}

func FromProject(p *project.Project, rev ReverseResolver) ProjectDTO {
	teamName, _ := rev.TeamName(p.Spec.TeamID)
	if teamName == "" {
		teamName = p.Spec.TeamID
	}
	wm := metaToWire(p.Meta)
	wm.Owner.Name = teamName
	return ProjectDTO{
		APIVersion: APIVersion,
		Kind:       "Project",
		Metadata:   wm,
		Spec: ProjectSpec{
			Team:    teamName,
			Enabled: p.Spec.Enabled,
			Budget:  fromBudget(p.Spec.Budget),
		},
	}
}

// ---------------------------------------------------------------------------
// ServiceAccount / Group
// ---------------------------------------------------------------------------

// ToServiceAccount resolves the owning project name → id and the optional
// policy override. Owner mirrors spec.project, so it is re-derived rather
// than read from the wire form.
func ToServiceAccount(d ServiceAccountDTO, idx Resolver) (*serviceaccount.ServiceAccount, error) {
	projectID, ok := idx.ProjectID(d.Spec.Project)
	if !ok {
		return nil, fmt.Errorf("serviceaccount %q: project %q not found", d.Metadata.Name, d.Spec.Project)
	}
	var policyID string
	if d.Spec.Policy != "" {
		policyID, ok = idx.PolicyID(d.Spec.Policy)
		if !ok {
			return nil, fmt.Errorf("serviceaccount %q: policy %q not found", d.Metadata.Name, d.Spec.Policy)
		}
	}
	sa := &serviceaccount.ServiceAccount{
		Meta: d.Metadata.toMeta(),
		Spec: serviceaccount.Spec{
			ProjectID: projectID,
			PolicyID:  policyID,
			Enabled:   d.Spec.Enabled,
		},
	}
	sa.StampOwner()
	return sa, nil
}

func FromServiceAccount(sa *serviceaccount.ServiceAccount, rev ReverseResolver) ServiceAccountDTO {
	projectName, _ := rev.ProjectName(sa.Spec.ProjectID)
	if projectName == "" {
		projectName = sa.Spec.ProjectID
	}
	policyName := ""
	if sa.Spec.PolicyID != "" {
		policyName, _ = rev.PolicyName(sa.Spec.PolicyID)
		if policyName == "" {
			policyName = sa.Spec.PolicyID
		}
	}
	wm := metaToWire(sa.Meta)
	wm.Owner.Name = projectName
	return ServiceAccountDTO{
		APIVersion: APIVersion,
		Kind:       "ServiceAccount",
		Metadata:   wm,
		Spec: ServiceAccountSpec{
			Project: projectName,
			Policy:  policyName,
			Enabled: sa.Spec.Enabled,
		},
	}
}

// ToGroup resolves member usernames → user ids.
func ToGroup(d GroupDTO, idx Resolver) (*group.Group, error) {
	m := d.Metadata.toMeta()
	// A Group is a grant target: a user-owned one named after an IdP group
	// would inherit that group's bindings, so the owner is fixed here.
	m.Owner = meta.Owner{Kind: meta.OwnerSystem}
	var memberIDs []string
	for _, username := range d.Spec.Members {
		id, ok := idx.UserID(username)
		if !ok {
			return nil, fmt.Errorf("group %q: user %q not found", d.Metadata.Name, username)
		}
		memberIDs = append(memberIDs, id)
	}
	return &group.Group{
		Meta: m,
		Spec: group.Spec{MemberIDs: memberIDs, Enabled: d.Spec.Enabled},
	}, nil
}

func FromGroup(g *group.Group, rev ReverseResolver) GroupDTO {
	var members []string
	for _, id := range g.Spec.MemberIDs {
		name, ok := rev.Username(id)
		if !ok {
			name = id
		}
		members = append(members, name)
	}
	return GroupDTO{
		APIVersion: APIVersion,
		Kind:       "Group",
		Metadata:   metaToWire(g.Meta),
		Spec:       GroupSpec{Members: members, Enabled: g.Spec.Enabled},
	}
}
