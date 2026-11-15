package team

import (
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/pkg/meta"
)

// ToDirectory is t as the directory stores it.
func (t *Team) ToDirectory() (*directory.Team, error) {
	return &directory.Team{
		Meta: meta.ToDirectory(t.Meta),
		Spec: directory.TeamSpec{Enabled: t.Spec.Enabled},
	}, nil
}

// FromDirectory is a directory row as a Team. A team written as system-owned reads as the organization's.
func FromDirectory(r *directory.Team) (*Team, error) {
	t := &Team{
		Meta: meta.FromDirectory(r.Meta),
		Spec: Spec{Enabled: r.Spec.Enabled},
	}
	t.Meta.Owner = t.Meta.Owner.AsGlobalScope()
	return t, nil
}
