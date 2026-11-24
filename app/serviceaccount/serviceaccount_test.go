package serviceaccount

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/pkg/meta"
)

const (
	projectID = "0195f8a0-0000-7000-8000-000000000001"
	otherID   = "0195f8a0-0000-7000-8000-000000000002"
	teamID    = "0195f8a0-0000-7000-8000-000000000003"
	userID    = "0195f8a0-0000-7000-8000-000000000004"
)

func TestStampOwner(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   ServiceAccount
		want ServiceAccount
	}{
		{"spec.projectId alone is the project owner",
			ServiceAccount{Spec: Spec{ProjectID: projectID}},
			ServiceAccount{Meta: meta.Metadata{Owner: meta.Owner{Kind: meta.OwnerProject, ID: projectID}}, Spec: Spec{ProjectID: projectID}}},
		{"a project owner without an id takes spec.projectId",
			ServiceAccount{Meta: meta.Metadata{Owner: meta.Owner{Kind: meta.OwnerProject}}, Spec: Spec{ProjectID: projectID}},
			ServiceAccount{Meta: meta.Metadata{Owner: meta.Owner{Kind: meta.OwnerProject, ID: projectID}}, Spec: Spec{ProjectID: projectID}}},
		{"a project owner fills spec.projectId and clears spec.teamId",
			ServiceAccount{Meta: meta.Metadata{Owner: meta.Owner{Kind: meta.OwnerProject, ID: projectID}}, Spec: Spec{TeamID: teamID}},
			ServiceAccount{Meta: meta.Metadata{Owner: meta.Owner{Kind: meta.OwnerProject, ID: projectID}}, Spec: Spec{ProjectID: projectID}}},
		{"a project owner spec.projectId contradicts is left for Validate",
			ServiceAccount{Meta: meta.Metadata{Owner: meta.Owner{Kind: meta.OwnerProject, ID: otherID}}, Spec: Spec{ProjectID: projectID}},
			ServiceAccount{Meta: meta.Metadata{Owner: meta.Owner{Kind: meta.OwnerProject, ID: otherID}}, Spec: Spec{ProjectID: projectID}}},
		{"a team owner fills spec.teamId",
			ServiceAccount{Meta: meta.Metadata{Owner: meta.Owner{Kind: meta.OwnerTeam, ID: teamID}}, Spec: Spec{TeamID: otherID}},
			ServiceAccount{Meta: meta.Metadata{Owner: meta.Owner{Kind: meta.OwnerTeam, ID: teamID}}, Spec: Spec{TeamID: teamID}}},
		{"a team owner leaves a stray spec.projectId for Validate",
			ServiceAccount{Meta: meta.Metadata{Owner: meta.Owner{Kind: meta.OwnerTeam, ID: teamID}}, Spec: Spec{ProjectID: projectID}},
			ServiceAccount{Meta: meta.Metadata{Owner: meta.Owner{Kind: meta.OwnerTeam, ID: teamID}}, Spec: Spec{ProjectID: projectID, TeamID: teamID}}},
		{"a user owner fills spec.userId and clears spec.teamId",
			ServiceAccount{Meta: meta.Metadata{Owner: meta.Owner{Kind: meta.OwnerUser, ID: userID}}, Spec: Spec{TeamID: teamID}},
			ServiceAccount{Meta: meta.Metadata{Owner: meta.Owner{Kind: meta.OwnerUser, ID: userID}}, Spec: Spec{UserID: userID}}},
		{"a project owner clears spec.userId",
			ServiceAccount{Meta: meta.Metadata{Owner: meta.Owner{Kind: meta.OwnerProject, ID: projectID}}, Spec: Spec{UserID: userID}},
			ServiceAccount{Meta: meta.Metadata{Owner: meta.Owner{Kind: meta.OwnerProject, ID: projectID}}, Spec: Spec{ProjectID: projectID}}},
		{"a user owner leaves a stray spec.projectId for Validate",
			ServiceAccount{Meta: meta.Metadata{Owner: meta.Owner{Kind: meta.OwnerUser, ID: otherID}}, Spec: Spec{ProjectID: projectID}},
			ServiceAccount{Meta: meta.Metadata{Owner: meta.Owner{Kind: meta.OwnerUser, ID: otherID}}, Spec: Spec{ProjectID: projectID, UserID: otherID}}},
		{"an owner of another kind is left for Validate",
			ServiceAccount{Meta: meta.Metadata{Owner: meta.Global}, Spec: Spec{ProjectID: projectID}},
			ServiceAccount{Meta: meta.Metadata{Owner: meta.Global}, Spec: Spec{ProjectID: projectID}}},
	} {
		got := tc.in
		got.StampOwner()
		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Errorf("%s (-want +got):\n%s", tc.name, diff)
		}
	}
}

func TestValidate(t *testing.T) {
	project := meta.Owner{Kind: meta.OwnerProject, ID: projectID}
	team := meta.Owner{Kind: meta.OwnerTeam, ID: teamID}
	for _, tc := range []struct {
		name  string
		owner meta.Owner
		spec  Spec
		want  string
	}{
		{"project owner mirrors projectId", project, Spec{ProjectID: projectID}, ""},
		{"project owner with the id left to be stamped", meta.Owner{Kind: meta.OwnerProject}, Spec{ProjectID: projectID}, ""},
		{"project owner with projectId left to be stamped", project, Spec{}, ""},
		{"project owner with neither id", meta.Owner{Kind: meta.OwnerProject}, Spec{}, `serviceaccount "indexer": a project owner needs metadata.owner.id or spec.projectId`},
		{"projectId that is no uuid", meta.Owner{Kind: meta.OwnerProject}, Spec{ProjectID: "ml"}, "Key: 'ServiceAccount.Spec.ProjectID' Error:Field validation for 'ProjectID' failed on the 'uuid' tag"},
		{"owner id drifted from projectId", meta.Owner{Kind: meta.OwnerProject, ID: otherID}, Spec{ProjectID: projectID}, `serviceaccount "indexer": owner.id "0195f8a0-0000-7000-8000-000000000002" does not match spec.projectId "0195f8a0-0000-7000-8000-000000000001"`},
		{"team owner", team, Spec{TeamID: teamID}, ""},
		{"team owner with teamId left to be stamped", team, Spec{}, ""},
		{"team owner with a teamId sent back that is no id", team, Spec{TeamID: "platform"}, ""},
		{"team owner and a projectId", team, Spec{ProjectID: projectID}, `serviceaccount "indexer": owner is team "0195f8a0-0000-7000-8000-000000000003" but spec.projectId names project "0195f8a0-0000-7000-8000-000000000001"; a service account has one owner`},
		{"team owner without an id", meta.Owner{Kind: meta.OwnerTeam}, Spec{}, `serviceaccount "indexer": a team owner needs metadata.owner.id`},
		{"user owner", meta.Owner{Kind: meta.OwnerUser, ID: userID}, Spec{UserID: userID}, ""},
		{"user owner with userId left to be stamped", meta.Owner{Kind: meta.OwnerUser, ID: userID}, Spec{}, ""},
		{"user owner and a projectId", meta.Owner{Kind: meta.OwnerUser, ID: userID}, Spec{ProjectID: projectID}, `serviceaccount "indexer": owner is user "0195f8a0-0000-7000-8000-000000000004" but spec.projectId names project "0195f8a0-0000-7000-8000-000000000001"; a service account has one owner`},
		{"user owner without an id", meta.Owner{Kind: meta.OwnerUser}, Spec{}, `serviceaccount "indexer": a user owner needs metadata.owner.id`},
		{"organization owner", meta.Global, Spec{}, `serviceaccount "indexer": owner.kind must be project, team or user, got "organization"; the admin token is no user, so a user owner needs metadata.owner.id`},
		{"system owner", meta.Owner{Kind: meta.OwnerSystem}, Spec{}, `serviceaccount "indexer": owner.kind must be project, team or user, got "system"`},
		{"no owner", meta.Owner{}, Spec{ProjectID: projectID}, `serviceaccount "indexer": owner.kind must be project, team or user, got ""`},
	} {
		sa := &ServiceAccount{Meta: meta.Metadata{ID: otherID, Name: "indexer", Owner: tc.owner}, Spec: tc.spec}
		got := ""
		if err := sa.Validate(); err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %s", tc.name, got, tc.want)
		}
	}
}

// A directory row's owner columns come back as the same owner and mirror, for either owner kind.
func TestDirectoryRoundTrip(t *testing.T) {
	no := false
	for _, sa := range []*ServiceAccount{
		{Meta: meta.Metadata{ID: otherID, Name: "in-project", Owner: meta.Owner{Kind: meta.OwnerProject, ID: projectID}}, Spec: Spec{ProjectID: projectID, Enabled: &no}},
		{Meta: meta.Metadata{ID: otherID, Name: "in-team", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: teamID}}, Spec: Spec{TeamID: teamID}},
		{Meta: meta.Metadata{ID: otherID, Name: "of-a-user", Owner: meta.Owner{Kind: meta.OwnerUser, ID: userID}}, Spec: Spec{UserID: userID}},
	} {
		row, err := sa.ToDirectory()
		if err != nil {
			t.Fatal(err)
		}
		row.StampOwner()
		if diff := cmp.Diff(directory.ServiceAccountSpec{ProjectID: sa.Spec.ProjectID, TeamID: sa.Spec.TeamID, UserID: sa.Spec.UserID, Enabled: sa.Spec.Enabled}, row.Spec); diff != "" {
			t.Errorf("%s: directory spec (-want +got):\n%s", sa.Meta.Name, diff)
		}
		back, err := FromDirectory(row)
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(sa, back); diff != "" {
			t.Errorf("%s: round trip (-want +got):\n%s", sa.Meta.Name, diff)
		}
	}
}
