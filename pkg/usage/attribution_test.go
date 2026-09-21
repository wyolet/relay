package usage

import (
	"testing"
	"time"
)

// The attribution dimensions group and filter exactly like the entity slugs
// above them, and the three id filters intersect (AND) rather than union.
func TestAttributionDimensions_FilterAndGroupBy(t *testing.T) {
	now := time.Now().UTC()
	events := []Event{
		{RequestID: "a", Timestamp: now, Status: 200,
			ProjectID: "p1", Project: "ml-search", TeamID: "t1", Team: "platform",
			PrincipalKind: "serviceaccount", PrincipalID: "sa1", Principal: "indexer",
			CredentialKind: "key", CredentialID: "k1"},
		{RequestID: "b", Timestamp: now, Status: 200,
			ProjectID: "p1", Project: "ml-search", TeamID: "t1", Team: "platform",
			PrincipalKind: "serviceaccount", PrincipalID: "sa2", Principal: "trainer",
			CredentialKind: "key", CredentialID: "k2"},
		{RequestID: "c", Timestamp: now, Status: 500,
			ProjectID: "p2", Project: "billing", TeamID: "t2", Team: "finance",
			PrincipalKind: "user", PrincipalID: "u1",
			CredentialKind: "key", CredentialID: "k3"},
	}

	for _, g := range []string{
		"project", "team", "principal",
		"project_id", "team_id", "principal_id", "credential_id",
	} {
		if !IsValidGroupBy(g) {
			t.Fatalf("IsValidGroupBy(%q) = false", g)
		}
	}

	byProject, err := Summarize(events, "project")
	if err != nil {
		t.Fatalf("Summarize project: %v", err)
	}
	if len(byProject.Rows) != 2 {
		t.Fatalf("project groups: want 2, got %+v", byProject.Rows)
	}
	if byProject.Rows[0].Group["project"] != "ml-search" || byProject.Rows[0].Requests != 2 {
		t.Fatalf("top project group: %+v", byProject.Rows[0])
	}

	byTeam, err := Summarize(events, "team")
	if err != nil {
		t.Fatalf("Summarize team: %v", err)
	}
	if len(byTeam.Rows) != 2 || byTeam.Rows[0].Group["team"] != "platform" {
		t.Fatalf("team groups: %+v", byTeam.Rows)
	}

	byPrincipal, err := Summarize(events, "principal_id")
	if err != nil {
		t.Fatalf("Summarize principal_id: %v", err)
	}
	if len(byPrincipal.Rows) != 3 {
		t.Fatalf("principal_id groups: want 3, got %+v", byPrincipal.Rows)
	}

	if _, err := Summarize(events, "nonsense"); err == nil {
		t.Fatal("Summarize with an unknown group_by: want error, got nil")
	}

	ts, err := Bucketize(events, time.Hour, "project")
	if err != nil {
		t.Fatalf("Bucketize project: %v", err)
	}
	if len(ts.Rows) != 2 || ts.Rows[0].Group["project"] != "ml-search" {
		t.Fatalf("timeseries project groups: %+v", ts.Rows)
	}
	if _, err := Bucketize(events, time.Hour, "nonsense"); err == nil {
		t.Fatal("Bucketize with an unknown group_by: want error, got nil")
	}

	got := FilterEvents(events, EventQuery{ProjectID: []string{"p1"}})
	if len(got) != 2 {
		t.Fatalf("project_id filter: want 2, got %d", len(got))
	}
	got = FilterEvents(events, EventQuery{PrincipalID: []string{"sa2", "u1"}})
	if len(got) != 2 {
		t.Fatalf("principal_id filter: want 2, got %d", len(got))
	}
	// Different dimensions intersect: p1 lives in t1, so pairing it with t2
	// matches nothing.
	got = FilterEvents(events, EventQuery{ProjectID: []string{"p1"}, TeamID: []string{"t1"}})
	if len(got) != 2 {
		t.Fatalf("project_id+team_id filter: want 2, got %d", len(got))
	}
	got = FilterEvents(events, EventQuery{ProjectID: []string{"p1"}, TeamID: []string{"t2"}})
	if len(got) != 0 {
		t.Fatalf("disjoint project_id+team_id filter: want 0, got %d", len(got))
	}
}
