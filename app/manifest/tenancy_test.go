package manifest_test

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/manifest"
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
spec:
  budget:
    amount: "2500.00"
    period: month
    onExceed: block
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
  budget: {amount: "500.00", period: month, onExceed: block}
`

const teamUUID = "0195f8a0-0000-7000-8000-000000000001"

var tenancyResolver = manifest.MapResolver{Teams: map[string]string{"platform": teamUUID}}
var tenancyRev = manifest.MapReverseResolver{Teams: map[string]string{teamUUID: "platform"}}

func TestRoundTrip_TeamAndProject(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(tenancyYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(docs) != 2 || docs[0].Team == nil || docs[1].Project == nil {
		t.Fatalf("want Team + Project docs, got %d: %v", len(docs), docs)
	}

	tm, err := manifest.ToTeam(*docs[0].Team, tenancyResolver)
	if err != nil {
		t.Fatalf("ToTeam: %v", err)
	}
	if tm.Meta.Annotations["wyolet.com/owner-email"] != "platform@example.com" {
		t.Errorf("team annotations = %v", tm.Meta.Annotations)
	}
	if tm.Spec.Budget == nil || tm.Spec.Budget.Amount != "2500.00" || tm.Spec.Budget.Period != "month" || tm.Spec.Budget.OnExceed != "block" {
		t.Errorf("team budget = %+v", tm.Spec.Budget)
	}
	if err := tm.Validate(); err != nil {
		t.Errorf("team Validate: %v", err)
	}
	back := manifest.FromTeam(tm, tenancyRev)
	if back.Metadata.Annotations["wyolet.com/owner-email"] != "platform@example.com" || back.Spec.Budget.Amount != "2500.00" {
		t.Errorf("team round-trip lost fields: %+v", back)
	}

	p, err := manifest.ToProject(*docs[1].Project, tenancyResolver)
	if err != nil {
		t.Fatalf("ToProject: %v", err)
	}
	if p.Spec.TeamID != teamUUID {
		t.Errorf("spec.team did not resolve: %q", p.Spec.TeamID)
	}
	if p.Meta.Owner.ID != teamUUID {
		t.Errorf("owner not stamped from spec.team: %+v", p.Meta.Owner)
	}
	if p.Meta.Annotations["wyolet.com/cost-center"] != "1042" {
		t.Errorf("project annotations = %v", p.Meta.Annotations)
	}
	if err := p.Validate(); err != nil {
		t.Errorf("project Validate: %v", err)
	}
	backP := manifest.FromProject(p, tenancyRev)
	if backP.Spec.Team != "platform" || backP.Spec.Budget.Amount != "500.00" ||
		backP.Metadata.Annotations["wyolet.com/cost-center"] != "1042" {
		t.Errorf("project round-trip lost fields: %+v", backP)
	}
}

func TestToProject_UnknownTeam(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(tenancyYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := manifest.ToProject(*docs[1].Project, manifest.MapResolver{}); err == nil {
		t.Fatal("want error for an unknown team name")
	}
}
