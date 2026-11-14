package modelref

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

// fiveShapes is one ref of every kind, broadest first.
var fiveShapes = []string{
	"anthropic",
	"@bedrock",
	"anthropic/claude-opus-4-7",
	"anthropic@bedrock",
	"anthropic/claude-opus-4-7@bedrock",
}

func TestFormat(t *testing.T) {
	for _, tc := range []struct{ provider, model, host, want, err string }{
		{"", "", "bedrock", "@bedrock", ""},
		{"anthropic", "", "", "anthropic", ""},
		{"anthropic", "", "bedrock", "anthropic@bedrock", ""},
		{"anthropic", "claude-opus-4-7", "", "anthropic/claude-opus-4-7", ""},
		{"anthropic", "claude-opus-4-7", "bedrock", "anthropic/claude-opus-4-7@bedrock", ""},
		{"", "claude-opus-4-7", "bedrock", "", "modelref.Format: model requires provider"},
		{"", "claude-opus-4-7", "", "", "modelref.Format: at least provider or host required"},
		{"", "", "", "", "modelref.Format: at least provider or host required"},
	} {
		got, err := Format(tc.provider, tc.model, tc.host)
		if got != tc.want || (err == nil) != (tc.err == "") || (err != nil && err.Error() != tc.err) {
			t.Errorf("Format(%q, %q, %q) = %q, %v; want %q, error %q", tc.provider, tc.model, tc.host, got, err, tc.want, tc.err)
		}
	}
	for _, s := range fiveShapes {
		r := MustParse(s)
		got, err := Format(refField(r.Provider, r.ProviderWildcard), refField(r.Model, r.ModelWildcard), refField(r.Host, r.HostWildcard))
		if err != nil || got != s {
			t.Errorf("Format of parsed %q = %q, %v", s, got, err)
		}
	}
}

// relation fills an n×n table by applying f to every ordered pair of fiveShapes.
func relation(f func(a, b Ref) bool) [][]bool {
	out := make([][]bool, len(fiveShapes))
	for i, a := range fiveShapes {
		out[i] = make([]bool, len(fiveShapes))
		for j, b := range fiveShapes {
			out[i][j] = f(MustParse(a), MustParse(b))
		}
	}
	return out
}

func TestIncludes(t *testing.T) {
	const y, n = true, false
	// Rows are the outer ref, columns the inner one, both in fiveShapes order.
	want := [][]bool{
		/* anthropic            */ {y, n, y, y, y},
		/* @bedrock             */ {n, y, n, y, y},
		/* anthropic/opus       */ {n, n, y, n, y},
		/* anthropic@bedrock    */ {n, n, n, y, y},
		/* anthropic/opus@bedr. */ {n, n, n, n, y},
	}
	if diff := cmp.Diff(want, relation(Includes)); diff != "" {
		t.Errorf("Includes(outer, inner) (-want +got):\n%s", diff)
	}
	for _, tc := range []struct {
		outer, inner string
		want         bool
	}{
		{"anthropic", "openai", false},
		{"anthropic", "openai/gpt-5", false},
		{"anthropic/claude-opus-4-7", "anthropic/claude-sonnet-4-6", false},
		{"@bedrock", "anthropic@vertex", false},
		{"anthropic/*", "anthropic/claude-opus-4-7", true},
	} {
		if got := Includes(MustParse(tc.outer), MustParse(tc.inner)); got != tc.want {
			t.Errorf("Includes(%q, %q) = %v, want %v", tc.outer, tc.inner, got, tc.want)
		}
	}
}

func TestOverlap(t *testing.T) {
	// The five shapes all admit anthropic/claude-opus-4-7@bedrock.
	for i, row := range relation(Overlap) {
		for j, got := range row {
			if !got {
				t.Errorf("Overlap(%q, %q) = false, want true", fiveShapes[i], fiveShapes[j])
			}
		}
	}
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"anthropic", "openai", false},
		{"anthropic/claude-opus-4-7", "anthropic/claude-sonnet-4-6", false},
		{"anthropic@bedrock", "openai@bedrock", false},
		{"anthropic@bedrock", "@vertex", false},
		{"anthropic/claude-opus-4-7@bedrock", "anthropic/claude-opus-4-7@vertex", false},
		{"anthropic/claude-opus-4-7", "@vertex", true},
		{"anthropic@bedrock", "anthropic/claude-sonnet-4-6", true},
	} {
		a, b := MustParse(tc.a), MustParse(tc.b)
		if got, back := Overlap(a, b), Overlap(b, a); got != tc.want || back != tc.want {
			t.Errorf("Overlap(%q, %q) = %v, reversed %v; want %v both ways", tc.a, tc.b, got, back, tc.want)
		}
	}
}

func TestSpecificity(t *testing.T) {
	got := make([]int, len(fiveShapes))
	for i, s := range fiveShapes {
		got[i] = Specificity(MustParse(s))
	}
	// Segment count first; at equal counts a host outranks a model.
	if diff := cmp.Diff([]int{10, 12, 21, 22, 33}, got); diff != "" {
		t.Errorf("Specificity of fiveShapes (-want +got):\n%s", diff)
	}
}

var algebraCatalog = []ConcreteBinding{
	{"anthropic", "claude-opus-4-7", "anthropic"},
	{"anthropic", "claude-opus-4-7", "bedrock"},
	{"anthropic", "claude-sonnet-4-6", "anthropic"},
	{"openai", "gpt-5", "openai"},
}

func TestOverlappingBindings(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want []ConcreteBinding
	}{
		{"anthropic", "@bedrock", []ConcreteBinding{{"anthropic", "claude-opus-4-7", "bedrock"}}},
		{"anthropic", "anthropic/claude-opus-4-7", []ConcreteBinding{{"anthropic", "claude-opus-4-7", "anthropic"}, {"anthropic", "claude-opus-4-7", "bedrock"}}},
		// Refs that could share a binding the catalog does not hold: empty, not nil.
		{"anthropic", "@vertex", []ConcreteBinding{}},
		// Refs that can share none: nil.
		{"anthropic", "openai", nil},
	} {
		got := OverlappingBindings(MustParse(tc.a), MustParse(tc.b), algebraCatalog)
		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Errorf("OverlappingBindings(%q, %q) (-want +got):\n%s", tc.a, tc.b, diff)
		}
	}
}

func TestResolve(t *testing.T) {
	refs := []Ref{MustParse("anthropic"), MustParse("openai/gpt-5"), MustParse("@vertex"), MustParse("Anthropic/*@Bedrock")}
	want := map[string][]ConcreteBinding{
		"anthropic":           {{"anthropic", "claude-opus-4-7", "anthropic"}, {"anthropic", "claude-opus-4-7", "bedrock"}, {"anthropic", "claude-sonnet-4-6", "anthropic"}},
		"openai/gpt-5":        {{"openai", "gpt-5", "openai"}},
		"@vertex":             {},
		"Anthropic/*@Bedrock": {{"anthropic", "claude-opus-4-7", "bedrock"}},
	}
	if diff := cmp.Diff(want, Resolve(refs, algebraCatalog)); diff != "" {
		t.Errorf("Resolve, keyed by the ref as written (-want +got):\n%s", diff)
	}
	if got := (ConcreteBinding{"anthropic", "claude-opus-4-7", "bedrock"}).String(); got != "anthropic/claude-opus-4-7@bedrock" {
		t.Errorf("ConcreteBinding.String() = %q", got)
	}
}
