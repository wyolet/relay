package project

import (
	"testing"

	"github.com/wyolet/relay/pkg/meta"
)

const (
	teamID  = "0195f8a0-0000-7000-8000-000000000001"
	otherID = "0195f8a0-0000-7000-8000-000000000002"
)

func TestStampOwner(t *testing.T) {
	p := &Project{Meta: meta.Metadata{Owner: meta.Owner{Kind: meta.OwnerUser, ID: otherID}}, Spec: Spec{TeamID: teamID}}
	p.StampOwner()
	if want := (meta.Owner{Kind: meta.OwnerTeam, ID: teamID}); p.Meta.Owner != want {
		t.Fatalf("owner = %+v, want %+v", p.Meta.Owner, want)
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(p *Project)
		want string
	}{
		{"team owner with the id left to be stamped", func(*Project) {}, ""},
		{"team owner id matching teamId", func(p *Project) { p.Meta.Owner.ID = teamID }, ""},
		{"owner id differing from teamId", func(p *Project) { p.Meta.Owner.ID = otherID }, `project "ml-search": owner.id "0195f8a0-0000-7000-8000-000000000002" does not match spec.teamId "0195f8a0-0000-7000-8000-000000000001"`},
		{"user owner", func(p *Project) { p.Meta.Owner.Kind = meta.OwnerUser }, `project "ml-search": owner.kind must be team, got "user"`},
		{"no owner", func(p *Project) { p.Meta.Owner = meta.Owner{} }, `project "ml-search": owner.kind must be team, got ""`},
		{"no teamId", func(p *Project) { p.Spec.TeamID = "" }, "Key: 'Project.Spec.TeamID' Error:Field validation for 'TeamID' failed on the 'required' tag"},
		{"teamId that is no uuid", func(p *Project) { p.Spec.TeamID = "platform" }, "Key: 'Project.Spec.TeamID' Error:Field validation for 'TeamID' failed on the 'uuid' tag"},
	} {
		p := &Project{Meta: meta.Metadata{ID: otherID, Name: "ml-search", Owner: meta.Owner{Kind: meta.OwnerTeam}}, Spec: Spec{TeamID: teamID}}
		tc.edit(p)
		got := ""
		if err := p.Validate(); err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %s", tc.name, got, tc.want)
		}
	}
}
