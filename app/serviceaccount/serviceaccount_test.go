package serviceaccount

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
)

func fix(name string) *ServiceAccount {
	sa := &ServiceAccount{
		Meta: meta.Metadata{ID: meta.NewID(), Name: name},
		Spec: Spec{ProjectID: meta.NewID()},
	}
	sa.StampOwner()
	return sa
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		sa   *ServiceAccount
		want string // "" = valid
	}{
		{
			name: "project owner mirrors projectId",
			sa:   fix("indexer"),
		},
		{
			name: "policy override",
			sa: func() *ServiceAccount {
				sa := fix("indexer")
				sa.Spec.PolicyID = meta.NewID()
				return sa
			}(),
		},
		{
			name: "missing projectId",
			sa: func() *ServiceAccount {
				sa := fix("indexer")
				sa.Spec.ProjectID = ""
				sa.StampOwner()
				return sa
			}(),
			want: "ProjectID",
		},
		{
			name: "user owner rejected",
			sa: func() *ServiceAccount {
				sa := fix("indexer")
				sa.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: meta.NewID()}
				return sa
			}(),
			want: "owner.kind must be project",
		},
		{
			name: "owner id drifted from projectId",
			sa: func() *ServiceAccount {
				sa := fix("indexer")
				sa.Meta.Owner.ID = meta.NewID()
				return sa
			}(),
			want: "does not match spec.projectId",
		},
		{
			name: "policyId must be a uuid",
			sa: func() *ServiceAccount {
				sa := fix("indexer")
				sa.Spec.PolicyID = "ml-pol"
				return sa
			}(),
			want: "PolicyID",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.sa.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("unexpected: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("got %v, want substring %q", err, tc.want)
			}
		})
	}
}
