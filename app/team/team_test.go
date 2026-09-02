package team

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
)

func fix(name string) *Team {
	return &Team{Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerUser}}}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		team *Team
		want string // "" = valid
	}{
		{
			name: "user owner",
			team: fix("platform"),
		},
		{
			name: "system owner",
			team: func() *Team {
				tm := fix("platform")
				tm.Meta.Owner.Kind = meta.OwnerSystem
				return tm
			}(),
		},
		{
			name: "provider owner rejected",
			team: func() *Team {
				tm := fix("platform")
				tm.Meta.Owner.Kind = meta.OwnerProvider
				return tm
			}(),
			want: "owner.kind must be user or system",
		},
		{
			name: "budget amount two decimals",
			team: func() *Team {
				tm := fix("platform")
				tm.Spec.Budget = &Budget{Amount: "12.5"}
				return tm
			}(),
		},
		{
			name: "budget amount three decimals rejected",
			team: func() *Team {
				tm := fix("platform")
				tm.Spec.Budget = &Budget{Amount: "12.505"}
				return tm
			}(),
			want: "budgetamount",
		},
		{
			name: "budget period year rejected",
			team: func() *Team {
				tm := fix("platform")
				tm.Spec.Budget = &Budget{Amount: "10", Period: "year"}
				return tm
			}(),
			want: "Period",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.team.Validate()
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
