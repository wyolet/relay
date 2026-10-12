package group

import (
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/auth/directory"
)

// ToDirectory is g as the directory stores it. External is left out: the directory keeps the stored one when a write carries none, and a caller must not be able to set it.
func (g *Group) ToDirectory() *directory.Group {
	return &directory.Group{
		Meta: meta.ToDirectory(g.Meta),
		Spec: directory.GroupSpec{MemberIDs: g.Spec.MemberIDs, Enabled: g.Spec.Enabled},
	}
}

// FromDirectory is a directory row as a Group.
func FromDirectory(r *directory.Group) *Group {
	g := &Group{
		Meta: meta.FromDirectory(r.Meta),
		Spec: Spec{MemberIDs: r.Spec.MemberIDs, Enabled: r.Spec.Enabled},
	}
	if !r.Spec.External.IsZero() {
		g.External = &External{Source: r.Spec.External.Source, ID: r.Spec.External.ID}
	}
	return g
}
