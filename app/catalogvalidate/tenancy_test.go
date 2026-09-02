package catalogvalidate

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/manifest"
)

func teamDoc(name string) manifest.Document {
	return manifest.Document{Team: &manifest.TeamDTO{
		APIVersion: "v1alpha2",
		Kind:       "Team",
		Metadata:   manifest.WireMeta{Name: name, Owner: manifest.WireOwner{Kind: "system"}},
	}}
}

func projectDoc(name, teamName string) manifest.Document {
	return manifest.Document{Project: &manifest.ProjectDTO{
		APIVersion: "v1alpha2",
		Kind:       "Project",
		Metadata:   manifest.WireMeta{Name: name, Owner: manifest.WireOwner{Kind: "team", Name: teamName}},
		Spec:       manifest.ProjectSpec{Team: teamName},
	}}
}

func TestValidateGraph_ProjectTeamRef(t *testing.T) {
	docs := append(fixture(), teamDoc("platform"), projectDoc("ml-search", "platform"))
	if issues := ValidateGraph(docs); HasErrors(issues) {
		t.Fatalf("project with a present team should be clean, got:\n%s", Format(issues))
	}

	docs = append(fixture(), projectDoc("ml-search", "platform"))
	issues := ValidateGraph(docs)
	if !HasErrors(issues) {
		t.Fatal("project with a missing team should error")
	}
	if !strings.Contains(Format(issues), `team "platform" not found`) {
		t.Errorf("unexpected report:\n%s", Format(issues))
	}
}

func TestValidateGraph_OwnerProjectRef(t *testing.T) {
	polOwnedBy := func(projectName string) manifest.Document {
		return manifest.Document{Policy: &manifest.PolicyDTO{
			APIVersion: "v1alpha2",
			Kind:       "Policy",
			Metadata:   manifest.WireMeta{Name: "team-pol", Owner: manifest.WireOwner{Kind: "project", Name: projectName}},
		}}
	}

	docs := append(fixture(), teamDoc("platform"), projectDoc("ml-search", "platform"), polOwnedBy("ml-search"))
	if issues := ValidateGraph(docs); HasErrors(issues) {
		t.Fatalf("project-owned policy with a present project should be clean, got:\n%s", Format(issues))
	}

	docs = append(fixture(), teamDoc("platform"), polOwnedBy("ml-search"))
	issues := ValidateGraph(docs)
	if !HasErrors(issues) {
		t.Fatal("project-owned policy with a missing project should error")
	}
	if !strings.Contains(Format(issues), `owner project "ml-search" not found`) {
		t.Errorf("unexpected report:\n%s", Format(issues))
	}

	rlOwnedBy := func(projectName string) manifest.Document {
		return manifest.Document{RateLimit: &manifest.RateLimitDTO{
			APIVersion: "v1alpha2",
			Kind:       "RateLimit",
			Metadata:   manifest.WireMeta{Name: "team-rl", Owner: manifest.WireOwner{Kind: "project", Name: projectName}},
		}}
	}

	docs = append(fixture(), teamDoc("platform"), projectDoc("ml-search", "platform"), rlOwnedBy("ml-search"))
	if issues := ValidateGraph(docs); HasErrors(issues) {
		t.Fatalf("project-owned rate limit with a present project should be clean, got:\n%s", Format(issues))
	}

	docs = append(fixture(), teamDoc("platform"), rlOwnedBy("ml-search"))
	issues = ValidateGraph(docs)
	if !HasErrors(issues) {
		t.Fatal("project-owned rate limit with a missing project should error")
	}
	if !strings.Contains(Format(issues), `RateLimit/team-rl.metadata.owner`) {
		t.Errorf("unexpected report:\n%s", Format(issues))
	}
}
