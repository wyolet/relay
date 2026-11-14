package model

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/meta"
)

// problems reports why Validate refused: "Namespace tag" per failed field rule, or the message of a kind-specific rule with the `model "m": ` prefix cut.
func problems(err error) []string {
	var fields validator.ValidationErrors
	if errors.As(err, &fields) {
		var out []string
		for _, f := range fields {
			out = append(out, f.Namespace()+" "+f.Tag())
		}
		return out
	}
	if err != nil {
		return []string{strings.TrimPrefix(err.Error(), `model "m": `)}
	}
	return nil
}

// fix is a minimal valid model named m with one snapshot, m-snap.
func fix() *Model {
	return &Model{
		Meta: meta.Metadata{Name: "m", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: meta.NewID()}},
		Spec: Spec{Snapshots: []Snapshot{{Name: "m-snap", OriginalName: "m.snap"}}, Pointer: "m-snap"},
	}
}

func TestValidate(t *testing.T) {
	long := strings.Repeat("a", 62)
	efforts := func(reasoning bool, def string, list ...string) func(*Model) {
		return func(m *Model) {
			m.Spec.Capabilities.Reasoning, m.Spec.Capabilities.DefaultReasoningEffort, m.Spec.Capabilities.ReasoningEfforts = reasoning, def, list
		}
	}
	aliases := func(list ...string) func(*Model) { return func(m *Model) { m.Spec.Aliases = list } }
	const levels = " (want one of none, minimal, low, medium, high, xhigh, max)"
	for _, tc := range []struct {
		name string
		edit func(*Model)
		want []string
	}{
		{"minimal", func(*Model) {}, nil},
		{"pointer matches a snapshot ignoring case", func(m *Model) { m.Spec.Pointer = "M-SNAP" }, nil},
		{"missing name", func(m *Model) { m.Meta.Name = "" }, []string{"Model.Meta.Name required"}},
		{"no snapshots, no pointer", func(m *Model) { m.Spec.Snapshots, m.Spec.Pointer = nil, "" }, []string{"Model.Spec.Snapshots required", "Model.Spec.Pointer required"}},
		{"missing pointer", func(m *Model) { m.Spec.Pointer = "" }, []string{"Model.Spec.Pointer required"}},
		{"snapshot name is not a hostname label", func(m *Model) { m.Spec.Snapshots[0].Name = "m snap" }, []string{"Model.Spec.Snapshots[0].Name hostname_rfc1123"}},
		{"unknown deprecation status", func(m *Model) { m.Spec.Deprecation = &Deprecation{Status: "bogus"} }, []string{"Model.Spec.Deprecation.Status oneof"}},
		{"duplicate snapshot, ignoring case", func(m *Model) { m.Spec.Snapshots = append(m.Spec.Snapshots, Snapshot{Name: "M-Snap"}) }, []string{`duplicate snapshot "M-Snap"`}},
		{"pointer matches no snapshot", func(m *Model) { m.Spec.Pointer = "nope" }, []string{`pointer "nope" does not match any snapshot`}},

		// A model is shared catalog data owned by its provider.
		{"provider owner without an id", func(m *Model) { m.Meta.Owner.ID = "" }, []string{"owner.id is required (provider id)"}},
		{"no owner", func(m *Model) { m.Meta.Owner = meta.Owner{} }, []string{`owner.kind must be provider, got ""`}},
		{"system owner", func(m *Model) { m.Meta.Owner.Kind = meta.OwnerSystem }, []string{`owner.kind must be provider, got "system"`}},
		{"user owner", func(m *Model) { m.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: "u-1"} }, []string{`owner.kind must be provider, got "user"`}},
		{"team owner", func(m *Model) { m.Meta.Owner = meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"} }, []string{`owner.kind must be provider, got "team"`}},
		{"project owner", func(m *Model) { m.Meta.Owner = meta.Owner{Kind: meta.OwnerProject, ID: "p-1"} }, []string{`owner.kind must be provider, got "project"`}},

		{"efforts unset on a non-reasoning model", efforts(false, ""), nil},
		{"efforts without a default", efforts(true, "", "low", "medium", "high"), nil},
		{"every effort, with a default", efforts(true, "high", "none", "minimal", "low", "medium", "high", "xhigh", "max"), nil},
		{"efforts may skip levels", efforts(true, "max", "low", "max"), nil},
		{"unknown effort", efforts(true, "", "low", "turbo"), []string{`unknown reasoning effort "turbo"` + levels}},
		{"efforts are case sensitive", efforts(true, "", "Low"), []string{`unknown reasoning effort "Low"` + levels}},
		{"duplicate effort", efforts(true, "", "low", "low"), []string{`duplicate reasoning effort "low"`}},
		{"efforts out of order", efforts(true, "", "high", "low"), []string{`reasoning effort "low" out of order — reasoningEfforts must be ordered low to high`}},
		{"default effort not listed", efforts(true, "max", "low", "high"), []string{`defaultReasoningEffort "max" is not in reasoningEfforts`}},
		{"default effort without a list", efforts(true, "high"), []string{`defaultReasoningEffort "high" is not in reasoningEfforts`}},
		{"efforts on a non-reasoning model", efforts(false, "", "low"), []string{"reasoningEfforts/defaultReasoningEffort require capabilities.reasoning"}},
		{"default effort on a non-reasoning model", efforts(false, "low"), []string{"reasoningEfforts/defaultReasoningEffort require capabilities.reasoning"}},

		{"exact and pattern aliases", aliases("m[1m]", "m[*]", "ft:gpt*:acme"), nil},
		{"empty alias", aliases(""), []string{"empty alias"}},
		{"bare star", aliases("*"), []string{`alias "*": pattern must start with a literal prefix`}},
		{"leading star", aliases("*-mini"), []string{`alias "*-mini": pattern must start with a literal prefix`}},
		{"two stars", aliases("a*b*c"), []string{`alias "a*b*c": at most one '*' is allowed`}},
		{"pattern prefix without usable characters", aliases("[[*x"), []string{`alias "[[*x": pattern prefix has no usable characters`}},
		{"alias without usable characters", aliases("[]"), []string{`alias "[]" has no usable characters`}},
		{"aliases equal once normalized", aliases("x[1m]", "x.1m"), []string{`duplicate alias "x.1m" (normalized form "x-1m")`}},
		{"patterns equal once normalized", aliases("x[*]", "x.*"), []string{`duplicate alias "x.*" (normalized form "x-*")`}},
		{"aliases equal once cut to the slug length", aliases(long+"[1m]", long+"[2m]"), []string{`duplicate alias "` + long + `[2m]" (normalized form "` + long + `")`}},
		{"alias shadows the model's own snapshot", aliases("m.snap"), []string{`alias "m.snap" normalizes to snapshot name "m-snap" — exact names always win, the alias would be dead`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := fix()
			tc.edit(m)
			if diff := cmp.Diff(tc.want, problems(m.Validate())); diff != "" {
				t.Errorf("Validate (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSnapshotUpstream(t *testing.T) {
	for _, tc := range []struct {
		s    Snapshot
		want string
	}{
		{Snapshot{Name: "gpt-4o-2024-11-20"}, "gpt-4o-2024-11-20"},
		{Snapshot{Name: "gpt-5-5", OriginalName: "gpt-5.5"}, "gpt-5.5"},
		{Snapshot{Name: "ollama-llama2-7b", OriginalName: "ollama/llama2:7b"}, "ollama/llama2:7b"},
	} {
		if got := tc.s.Upstream(); got != tc.want {
			t.Errorf("Upstream() of %+v = %q, want %q", tc.s, got, tc.want)
		}
	}
}

func TestAliasPattern(t *testing.T) {
	for _, tc := range []struct {
		alias, prefix, suffix string
		isPattern             bool
	}{
		{"claude-fable-5[*]", "claude-fable-5-", "", true},
		{"ft:gpt*:acme", "ft-gpt", "-acme", true},
		{"GPT*", "gpt", "", true},
		{"*", "", "", true},
		{"claude-fable-5[1m]", "", "", false},
		{"", "", "", false},
	} {
		prefix, suffix, ok := AliasPattern(tc.alias)
		if ok != tc.isPattern || prefix != tc.prefix || suffix != tc.suffix {
			t.Errorf("AliasPattern(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.alias, prefix, suffix, ok, tc.prefix, tc.suffix, tc.isPattern)
		}
	}
}

func TestIsEnabled(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		val  *bool
		want bool
	}{{nil, true}, {&yes, true}, {&no, false}} {
		if got := (&Model{Spec: Spec{Enabled: tc.val}}).IsEnabled(); got != tc.want {
			t.Errorf("IsEnabled with enabled %v = %v, want %v", tc.val, got, tc.want)
		}
	}
}
