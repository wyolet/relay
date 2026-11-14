package manifest_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/meta"
)

const tenancyYAML = `
apiVersion: relay.wyolet.dev/v1alpha2
kind: Team
metadata:
  name: platform
  displayName: Platform Engineering
  owner: {kind: system}
  annotations:
    wyolet.com/owner-email: platform@example.com
spec: {}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Project
metadata:
  name: ml-search
  displayName: ML Search
  annotations:
    wyolet.com/cost-center: "1042"
spec:
  team: platform
`

const teamUUID = "0195f8a0-0000-7000-8000-000000000001"

var tenancyResolver = manifest.MapResolver{Teams: map[string]string{"platform": teamUUID}}
var tenancyRev = manifest.MapReverseResolver{Teams: map[string]string{teamUUID: "platform"}}

// A project names its team, which becomes its owner; annotations survive both directions.
func TestTeamAndProject(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(tenancyYAML))
	if err != nil || len(docs) != 2 || docs[0].Team == nil || docs[1].Project == nil {
		t.Fatalf("Parse = %+v, %v; want a Team then a Project", docs, err)
	}

	tm, err := manifest.ToTeam(*docs[0].Team, tenancyResolver)
	if err != nil {
		t.Fatalf("ToTeam: %v", err)
	}
	wantTeam := &team.Team{Meta: meta.Metadata{Name: "platform", DisplayName: "Platform Engineering", Owner: meta.Owner{Kind: meta.OwnerSystem},
		Annotations: map[string]string{"wyolet.com/owner-email": "platform@example.com"}}}
	if diff := cmp.Diff(wantTeam, tm); diff != "" {
		t.Errorf("ToTeam (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(docs[0].Team.Metadata, manifest.FromTeam(tm, tenancyRev).Metadata); diff != "" {
		t.Errorf("FromTeam metadata (-document +export):\n%s", diff)
	}

	p, err := manifest.ToProject(*docs[1].Project, tenancyResolver)
	if err != nil {
		t.Fatalf("ToProject: %v", err)
	}
	wantProject := &project.Project{
		Meta: meta.Metadata{Name: "ml-search", DisplayName: "ML Search", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: teamUUID},
			Annotations: map[string]string{"wyolet.com/cost-center": "1042"}},
		Spec: project.Spec{TeamID: teamUUID},
	}
	if diff := cmp.Diff(wantProject, p); diff != "" {
		t.Errorf("ToProject (-want +got):\n%s", diff)
	}
	back := manifest.FromProject(p, tenancyRev)
	if back.Spec.Team != "platform" || back.Metadata.Annotations["wyolet.com/cost-center"] != "1042" || back.Metadata.Name != "ml-search" {
		t.Errorf("FromProject = %+v, want the team by name and the annotations kept", back)
	}

	_, err = manifest.ToProject(*docs[1].Project, manifest.MapResolver{})
	var notFound *manifest.RefNotFoundError
	if want := `project "ml-search": team "platform" not found`; err == nil || err.Error() != want || !errors.As(err, &notFound) {
		t.Errorf("an unknown team: err = %v, want RefNotFoundError %s", err, want)
	}
}
