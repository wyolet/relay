package team

import (
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/auth/directory"
)

// ToDirectory is t as the directory stores it.
func (t *Team) ToDirectory() (*directory.Team, error) {
	return &directory.Team{
		Meta: meta.ToDirectory(t.Meta),
		Spec: directory.TeamSpec{Enabled: t.Spec.Enabled},
	}, nil
}

// FromDirectory is a directory row as a Team.
func FromDirectory(r *directory.Team) (*Team, error) {
	return &Team{
		Meta: meta.FromDirectory(r.Meta),
		Spec: Spec{Enabled: r.Spec.Enabled},
	}, nil
}
