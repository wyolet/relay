package catalogvalidate

import (
	"testing"

	"github.com/wyolet/relay/app/manifest"
)

func TestRunRules_AppendsIssuesInOrder(t *testing.T) {
	rules := []Rule{
		{
			Name:     "alpha",
			Severity: SeverityWarning,
			Check: func(_ []manifest.Document) []Issue {
				return []Issue{{Severity: SeverityWarning, Kind: KindOrphan, Source: Ref{Kind: "Test", Name: "a"}, Message: "from alpha"}}
			},
		},
		{
			Name:     "beta",
			Severity: SeverityError,
			Check: func(_ []manifest.Document) []Issue {
				return []Issue{{Severity: SeverityError, Kind: KindRefMissing, Source: Ref{Kind: "Test", Name: "b"}, Message: "from beta"}}
			},
		},
	}
	got := RunRules(rules, nil, nil)
	if len(got) != 2 {
		t.Fatalf("expected 2 issues, got %d", len(got))
	}
	// sortIssues puts errors before warnings.
	if got[0].Severity != SeverityError || got[1].Severity != SeverityWarning {
		t.Fatalf("expected errors-first ordering, got severities %v %v", got[0].Severity, got[1].Severity)
	}
}

func TestRunRules_SkipSuppresses(t *testing.T) {
	rules := []Rule{
		{
			Name: "alpha",
			Check: func(_ []manifest.Document) []Issue {
				return []Issue{{Severity: SeverityError, Message: "should not see this"}}
			},
		},
	}
	got := RunRules(rules, nil, map[string]bool{"alpha": true})
	if len(got) != 0 {
		t.Fatalf("expected skipped rule to emit nothing, got %d issues", len(got))
	}
}

func TestRunRules_NilCheckIgnored(t *testing.T) {
	rules := []Rule{{Name: "alpha", Check: nil}}
	got := RunRules(rules, nil, nil)
	if got != nil && len(got) != 0 {
		t.Fatalf("nil Check must not panic or emit; got %d issues", len(got))
	}
}

func TestPromote_WarningsBecomeErrors(t *testing.T) {
	in := []Issue{
		{Severity: SeverityWarning, Message: "w"},
		{Severity: SeverityError, Message: "e"},
	}
	got := Promote(in)
	for i, is := range got {
		if is.Severity != SeverityError {
			t.Fatalf("issue %d should be error, got %v", i, is.Severity)
		}
	}
	// Promote must not mutate input.
	if in[0].Severity != SeverityWarning {
		t.Fatalf("Promote mutated input")
	}
}

func TestListRules_SortsByName(t *testing.T) {
	in := []Rule{{Name: "zebra"}, {Name: "alpha"}, {Name: "mango"}}
	got := ListRules(in)
	want := []string{"alpha", "mango", "zebra"}
	for i, r := range got {
		if r.Name != want[i] {
			t.Fatalf("position %d: want %s, got %s", i, want[i], r.Name)
		}
	}
	// Must not mutate input.
	if in[0].Name != "zebra" {
		t.Fatalf("ListRules mutated input")
	}
}
