package group

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
)

func fix(name string) *Group {
	return &Group{Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerSystem}}}
}

func TestValidate(t *testing.T) {
	alice, bob := meta.NewID(), meta.NewID()
	for _, tc := range []struct {
		name  string
		group *Group
		want  string // "" = valid
	}{
		{
			name:  "system owner",
			group: fix("data-science"),
		},
		{
			name: "user owner",
			group: func() *Group {
				g := fix("data-science")
				g.Meta.Owner.Kind = meta.OwnerUser
				return g
			}(),
		},
		{
			name: "members",
			group: func() *Group {
				g := fix("data-science")
				g.Spec.MemberIDs = []string{alice, bob}
				return g
			}(),
		},
		{
			name: "team owner rejected",
			group: func() *Group {
				g := fix("data-science")
				g.Meta.Owner.Kind = meta.OwnerTeam
				return g
			}(),
			want: "owner.kind must be user or system",
		},
		{
			name:  "reserved system: name rejected",
			group: fix("system:authenticated"),
			want:  "reserved for built-in groups",
		},
		{
			name: "duplicate member rejected",
			group: func() *Group {
				g := fix("data-science")
				g.Spec.MemberIDs = []string{alice, alice}
				return g
			}(),
			want: "duplicate member id",
		},
		{
			name: "member id must be a uuid",
			group: func() *Group {
				g := fix("data-science")
				g.Spec.MemberIDs = []string{"alice"}
				return g
			}(),
			want: "MemberIDs",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.group.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("unexpected: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("got %v, want substring %q", err, tc.want)
			}
		})
	}
}
