package group

import (
	"encoding/json"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/pkg/meta"
)

// ToDirectory is g as the directory stores it. External is left out: the directory keeps the stored one when a write carries none, and a caller must not be able to set it.
func (g *Group) ToDirectory() *directory.Group {
	d := &directory.Group{
		Meta: meta.ToDirectory(g.Meta),
		Spec: directory.GroupSpec{MemberIDs: g.Spec.MemberIDs, Enabled: g.Spec.Enabled},
	}
	if g.Spec.PersonalAccount != "" {
		raw, _ := json.Marshal(g.Spec.PersonalAccount)
		d.Spec.Attributes = map[string]json.RawMessage{PersonalAccountAttribute: raw}
	}
	return d
}

// FromDirectory is a directory row as a Group.
func FromDirectory(r *directory.Group) *Group {
	g := &Group{
		Meta: meta.FromDirectory(r.Meta),
		Spec: Spec{MemberIDs: r.Spec.MemberIDs, Enabled: r.Spec.Enabled},
	}
	if raw, ok := r.Spec.Attributes[PersonalAccountAttribute]; ok {
		_ = json.Unmarshal(raw, &g.Spec.PersonalAccount)
	}
	if !r.Spec.External.IsZero() {
		g.External = &External{Source: r.Spec.External.Source, ID: r.Spec.External.ID}
	}
	return g
}
