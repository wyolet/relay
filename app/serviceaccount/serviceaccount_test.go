package serviceaccount

import (
	"testing"

	"github.com/wyolet/relay/pkg/meta"
)

const (
	projectID = "0195f8a0-0000-7000-8000-000000000001"
	otherID   = "0195f8a0-0000-7000-8000-000000000002"
)

func TestStampOwner(t *testing.T) {
	sa := &ServiceAccount{Meta: meta.Metadata{Owner: meta.Owner{Kind: meta.OwnerUser, ID: otherID}}, Spec: Spec{ProjectID: projectID}}
	sa.StampOwner()
	if want := (meta.Owner{Kind: meta.OwnerProject, ID: projectID}); sa.Meta.Owner != want {
		t.Fatalf("owner = %+v, want %+v", sa.Meta.Owner, want)
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(sa *ServiceAccount)
		want string
	}{
		{"project owner mirrors projectId", func(*ServiceAccount) {}, ""},
		{"project owner with the id left to be stamped", func(sa *ServiceAccount) { sa.Meta.Owner.ID = "" }, ""},
		{"no projectId", func(sa *ServiceAccount) { sa.Spec.ProjectID = "" }, "Key: 'ServiceAccount.Spec.ProjectID' Error:Field validation for 'ProjectID' failed on the 'required' tag"},
		{"projectId that is no uuid", func(sa *ServiceAccount) { sa.Spec.ProjectID = "ml" }, "Key: 'ServiceAccount.Spec.ProjectID' Error:Field validation for 'ProjectID' failed on the 'uuid' tag"},
		{"user owner", func(sa *ServiceAccount) { sa.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: projectID} }, `serviceaccount "indexer": owner.kind must be project, got "user"`},
		{"no owner", func(sa *ServiceAccount) { sa.Meta.Owner = meta.Owner{} }, `serviceaccount "indexer": owner.kind must be project, got ""`},
		{"owner id drifted from projectId", func(sa *ServiceAccount) { sa.Meta.Owner.ID = otherID }, `serviceaccount "indexer": owner.id "0195f8a0-0000-7000-8000-000000000002" does not match spec.projectId "0195f8a0-0000-7000-8000-000000000001"`},
	} {
		sa := &ServiceAccount{Meta: meta.Metadata{ID: otherID, Name: "indexer", Owner: meta.Owner{Kind: meta.OwnerProject, ID: projectID}}, Spec: Spec{ProjectID: projectID}}
		tc.edit(sa)
		got := ""
		if err := sa.Validate(); err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %s", tc.name, got, tc.want)
		}
	}
}
