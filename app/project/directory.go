package project

import (
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/auth/directory"
)

// ToDirectory is p as the directory stores it.
func (p *Project) ToDirectory() (*directory.Project, error) {
	attrs, err := team.WithBudget(nil, p.Spec.Budget)
	if err != nil {
		return nil, err
	}
	return &directory.Project{
		Meta: meta.ToDirectory(p.Meta),
		Spec: directory.ProjectSpec{TeamID: p.Spec.TeamID, Enabled: p.Spec.Enabled, Attributes: attrs},
	}, nil
}

// FromDirectory is a directory row as a Project.
func FromDirectory(r *directory.Project) (*Project, error) {
	b, err := team.BudgetAttribute(r.Spec.Attributes)
	if err != nil {
		return nil, err
	}
	p := &Project{
		Meta: meta.FromDirectory(r.Meta),
		Spec: Spec{TeamID: r.Spec.TeamID, Enabled: r.Spec.Enabled, Budget: b},
	}
	p.StampOwner()
	return p, nil
}
