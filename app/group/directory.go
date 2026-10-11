package group

import (
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/auth/directory"
)

// ToDirectory is g as the directory stores it.
func (g *Group) ToDirectory() *directory.Group {
	return &directory.Group{
		Meta: meta.ToDirectory(g.Meta),
		Spec: directory.GroupSpec{MemberIDs: g.Spec.MemberIDs, Enabled: g.Spec.Enabled},
	}
}

// FromDirectory is a directory row as a Group.
func FromDirectory(r *directory.Group) *Group {
	return &Group{
		Meta: meta.FromDirectory(r.Meta),
		Spec: Spec{MemberIDs: r.Spec.MemberIDs, Enabled: r.Spec.Enabled},
	}
}
