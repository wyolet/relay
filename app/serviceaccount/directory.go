package serviceaccount

import (
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/pkg/meta"
)

// ToDirectory is s as the directory stores it.
func (s *ServiceAccount) ToDirectory() (*directory.ServiceAccount, error) {
	return &directory.ServiceAccount{
		Meta: meta.ToDirectory(s.Meta),
		Spec: directory.ServiceAccountSpec{ProjectID: s.Spec.ProjectID, TeamID: s.Spec.TeamID, UserID: s.Spec.UserID, Enabled: s.Spec.Enabled},
	}, nil
}

// FromDirectory is a directory row as a ServiceAccount. Attributes relay no
// longer reads (a pre-v0.20 policy override) are ignored.
func FromDirectory(r *directory.ServiceAccount) (*ServiceAccount, error) {
	sa := &ServiceAccount{
		Meta: meta.FromDirectory(r.Meta),
		Spec: Spec{ProjectID: r.Spec.ProjectID, TeamID: r.Spec.TeamID, UserID: r.Spec.UserID, Enabled: r.Spec.Enabled},
	}
	sa.StampOwner()
	return sa, nil
}
