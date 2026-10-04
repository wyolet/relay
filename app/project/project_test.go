package project

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
)

const teamID = "0195f8a0-0000-7000-8000-000000000001"

func fix(name string) *Project {
	return &Project{
		Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerTeam}},
		Spec: Spec{TeamID: teamID},
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		project *Project
		want    string // "" = valid
	}{
		{
			name:    "team owner with empty id",
			project: fix("ml-search"),
		},
		{
			name: "team owner id matching teamId",
			project: func() *Project {
				p := fix("ml-search")
				p.Meta.Owner.ID = teamID
				return p
			}(),
		},
		{
			name: "owner id differing from teamId rejected",
			project: func() *Project {
				p := fix("ml-search")
				p.Meta.Owner.ID = "0195f8a0-0000-7000-8000-000000000002"
				return p
			}(),
			want: "does not match spec.teamId",
		},
		{
			name: "user owner rejected",
			project: func() *Project {
				p := fix("ml-search")
				p.Meta.Owner.Kind = meta.OwnerUser
				return p
			}(),
			want: "owner.kind must be team",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.project.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("got %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestStampOwner(t *testing.T) {
	p := fix("ml-search")
	p.Meta.Owner = meta.Owner{}
	p.StampOwner()
	if p.Meta.Owner.Kind != meta.OwnerTeam || p.Meta.Owner.ID != teamID {
		t.Fatalf("owner = %+v, want {team %s}", p.Meta.Owner, teamID)
	}
}
