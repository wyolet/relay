package catalogvalidate

import (
	"sort"

	"github.com/wyolet/relay/app/manifest"
)

// Rule is a named, documented check function. Catalog repos (like
// wyolet/relay-catalog) declare their curation conventions as a []Rule
// slice and pass it to RunRules. ValidateGraph is the schema-generic
// counterpart; Rules layer on catalog-specific data conventions.
//
// Conventions for authoring a Rule:
//
//   - Name is a stable kebab-case id ("ollama-source-tag"). It's the
//     handle for --skip flags, ignore files, and bug reports — don't
//     rename without a deprecation note.
//   - Description is a single sentence summarizing the rule. Renders in
//     --list output and (eventually) auto-generated docs.
//   - Severity is the default; CLI flags like --strict can override.
//     Individual issues emitted by Check may set their own severity to
//     downgrade or upgrade ad-hoc.
//   - Check is pure — no IO, no panics. Re-entrant; called in iteration
//     order of the rules slice.
type Rule struct {
	Name        string
	Description string
	Severity    Severity
	Check       func([]manifest.Document) []Issue
}

// RunRules executes every rule in order against docs and returns the
// flattened []Issue, sorted deterministically. Rules whose Name appears
// in skip are silently excluded.
//
// Rule.Severity is the "default" severity used in --list output and
// documentation; rule Check functions set per-issue severity explicitly.
// CLI flags like --strict can post-process the result (promote all
// warnings to errors) without touching this function.
func RunRules(rules []Rule, docs []manifest.Document, skip map[string]bool) []Issue {
	var out []Issue
	for _, r := range rules {
		if r.Check == nil {
			continue
		}
		if skip[r.Name] {
			continue
		}
		out = append(out, r.Check(docs)...)
	}
	sortIssues(out)
	return out
}

// Promote returns a copy of issues with every warning rewritten to an
// error. Useful for --strict CLI flags on a release build.
func Promote(issues []Issue) []Issue {
	out := make([]Issue, len(issues))
	for i, is := range issues {
		if is.Severity == SeverityWarning {
			is.Severity = SeverityError
		}
		out[i] = is
	}
	return out
}

// ListRules returns rules sorted by Name for stable --list output.
func ListRules(rules []Rule) []Rule {
	out := make([]Rule, len(rules))
	copy(out, rules)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
