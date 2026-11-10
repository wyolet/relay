package project

import (
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/auth/directory"
)

// ToDirectory is p as the directory stores it.
func (p *Project) ToDirectory() (*directory.Project, error) {
	return &directory.Project{
		Meta: meta.ToDirectory(p.Meta),
		Spec: directory.ProjectSpec{TeamID: p.Spec.TeamID, Enabled: p.Spec.Enabled},
	}, nil
}

// FromDirectory is a directory row as a Project.
func FromDirectory(r *directory.Project) (*Project, error) {
	p := &Project{
		Meta: meta.FromDirectory(r.Meta),
		Spec: Spec{TeamID: r.Spec.TeamID, Enabled: r.Spec.Enabled},
	}
	p.StampOwner()
	return p, nil
}
