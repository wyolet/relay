package modelref

import (
	"reflect"
	"strings"
	"testing"
)

// ---------- Parse / kind / wildcard bits ----------

func TestParse_FiveShapes(t *testing.T) {
	cases := []struct {
		in     string
		wantK  Kind
		wantP  string
		wantM  string
		wantH  string
		pWild  bool
		mWild  bool
		hWild  bool
	}{
		{"anthropic", KindProvider, "anthropic", "", "", false, true, true},
		{"@bedrock", KindHost, "", "", "bedrock", true, true, false},
		{"anthropic@bedrock", KindProviderOnHost, "anthropic", "", "bedrock", false, true, false},
		{"anthropic/claude-opus-4-7", KindModel, "anthropic", "claude-opus-4-7", "", false, false, true},
		{"anthropic/claude-opus-4-7@bedrock", KindBinding, "anthropic", "claude-opus-4-7", "bedrock", false, false, false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			r, err := Parse(c.in)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if r.Kind() != c.wantK {
				t.Errorf("kind=%q, want %q", r.Kind(), c.wantK)
			}
			if r.Provider != c.wantP || r.Model != c.wantM || r.Host != c.wantH {
				t.Errorf("segments=(%q,%q,%q), want (%q,%q,%q)", r.Provider, r.Model, r.Host, c.wantP, c.wantM, c.wantH)
			}
			if r.ProviderWildcard != c.pWild || r.ModelWildcard != c.mWild || r.HostWildcard != c.hWild {
				t.Errorf("wildcards=(%v,%v,%v), want (%v,%v,%v)",
					r.ProviderWildcard, r.ModelWildcard, r.HostWildcard, c.pWild, c.mWild, c.hWild)
			}
		})
	}
}

// ---------- Validate rejects bad input ----------

func TestValidate_Rejects(t *testing.T) {
	bad := []string{"", "anthropic*", "*", "/anthropic", "anthropic/", "anthropic@", "Anthropic", "@", "anthropic/Model"}
	for _, s := range bad {
		if err := Validate(s); err == nil {
			t.Errorf("Validate(%q) = nil, want error", s)
		}
	}
}

// ---------- Format ----------

func TestFormat(t *testing.T) {
	cases := []struct {
		p, m, h, want string
		err           bool
	}{
		{"", "", "bedrock", "@bedrock", false},
		{"anthropic", "", "", "anthropic", false},
		{"anthropic", "", "bedrock", "anthropic@bedrock", false},
		{"anthropic", "claude-opus-4-7", "", "anthropic/claude-opus-4-7", false},
		{"anthropic", "claude-opus-4-7", "bedrock", "anthropic/claude-opus-4-7@bedrock", false},
		{"", "claude-opus-4-7", "bedrock", "", true}, // model without provider
		{"", "", "", "", true},                       // nothing
	}
	for _, c := range cases {
		got, err := Format(c.p, c.m, c.h)
		if c.err {
			if err == nil {
				t.Errorf("Format(%q,%q,%q) = %q, want error", c.p, c.m, c.h, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Format(%q,%q,%q): %v", c.p, c.m, c.h, err)
			continue
		}
		if got != c.want {
			t.Errorf("Format(%q,%q,%q) = %q, want %q", c.p, c.m, c.h, got, c.want)
		}
	}
}

func TestFormat_RoundTripsParse(t *testing.T) {
	shapes := []string{
		"anthropic",
		"@bedrock",
		"anthropic@bedrock",
		"anthropic/claude-opus-4-7",
		"anthropic/claude-opus-4-7@bedrock",
	}
	for _, s := range shapes {
		r, err := Parse(s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		got, err := Format(
			refField(r.Provider, r.ProviderWildcard),
			refField(r.Model, r.ModelWildcard),
			refField(r.Host, r.HostWildcard),
		)
		if err != nil {
			t.Fatalf("format %q: %v", s, err)
		}
		if got != s {
			t.Errorf("round-trip %q -> %q", s, got)
		}
	}
}

// ---------- Covers ----------

func TestCovers(t *testing.T) {
	bind := ConcreteBinding{Provider: "anthropic", Model: "claude-opus-4-7", Host: "bedrock"}
	yes := []string{"anthropic", "@bedrock", "anthropic@bedrock", "anthropic/claude-opus-4-7", "anthropic/claude-opus-4-7@bedrock"}
	no := []string{"openai", "@vertex", "anthropic/claude-sonnet-4-6", "anthropic@vertex"}
	for _, s := range yes {
		r := MustParse(s)
		if !r.Covers(bind) {
			t.Errorf("%q should cover %v", s, bind)
		}
	}
	for _, s := range no {
		r := MustParse(s)
		if r.Covers(bind) {
			t.Errorf("%q should NOT cover %v", s, bind)
		}
	}
}

// ---------- Includes ----------

func TestIncludes(t *testing.T) {
	cases := []struct {
		outer, inner string
		want         bool
	}{
		{"anthropic", "anthropic", true}, // reflexive
		{"anthropic", "anthropic/claude-opus-4-7", true},
		{"anthropic", "anthropic@bedrock", true},
		{"anthropic", "anthropic/claude-opus-4-7@bedrock", true},
		{"anthropic/claude-opus-4-7", "anthropic", false},
		{"anthropic/claude-opus-4-7", "anthropic/claude-opus-4-7@bedrock", true},
		{"@bedrock", "anthropic/claude-opus-4-7@bedrock", true},
		{"@bedrock", "anthropic", false}, // orthogonal axes
		{"anthropic", "@bedrock", false},
		{"@bedrock", "@bedrock", true},
	}
	for _, c := range cases {
		o, i := MustParse(c.outer), MustParse(c.inner)
		if got := Includes(o, i); got != c.want {
			t.Errorf("Includes(%q, %q) = %v, want %v", c.outer, c.inner, got, c.want)
		}
	}
}

// ---------- Overlap ----------

func TestOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"anthropic", "openai", false},
		{"anthropic", "anthropic", true},
		{"anthropic", "@bedrock", true},
		{"anthropic/claude-opus-4-7", "anthropic/claude-sonnet-4-6", false},
		{"anthropic@bedrock", "anthropic/claude-opus-4-7", true},
		{"anthropic@bedrock", "openai@bedrock", false},
		{"anthropic@bedrock", "@vertex", false},
	}
	for _, c := range cases {
		a, b := MustParse(c.a), MustParse(c.b)
		if got := Overlap(a, b); got != c.want {
			t.Errorf("Overlap(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
		if got := Overlap(b, a); got != c.want {
			t.Errorf("Overlap symmetric (%q, %q) = %v, want %v", c.b, c.a, got, c.want)
		}
	}
}

// ---------- OverlappingBindings ----------

func TestOverlappingBindings(t *testing.T) {
	catalog := []ConcreteBinding{
		{"anthropic", "claude-opus-4-7", "anthropic"},
		{"anthropic", "claude-opus-4-7", "bedrock"},
		{"anthropic", "claude-sonnet-4-6", "anthropic"},
		{"openai", "gpt-5", "openai"},
	}
	a := MustParse("anthropic")
	b := MustParse("@bedrock")
	got := OverlappingBindings(a, b, catalog)
	want := []ConcreteBinding{{"anthropic", "claude-opus-4-7", "bedrock"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	// Disjoint refs → nil.
	if OverlappingBindings(MustParse("anthropic"), MustParse("openai"), catalog) != nil {
		t.Error("disjoint refs should return nil")
	}
	// Conceptually overlap but no catalog row → empty (non-nil).
	got = OverlappingBindings(MustParse("anthropic"), MustParse("@vertex"), catalog)
	if got == nil || len(got) != 0 {
		t.Errorf("empty match should return empty slice, got %v", got)
	}
}

// ---------- Specificity ----------

func TestSpecificity(t *testing.T) {
	cases := map[string]int{
		"anthropic":                         10,
		"@bedrock":                          12,
		"anthropic/claude-opus-4-7":         21,
		"anthropic@bedrock":                 22,
		"anthropic/claude-opus-4-7@bedrock": 33,
	}
	for s, want := range cases {
		r := MustParse(s)
		if got := Specificity(r); got != want {
			t.Errorf("Specificity(%q) = %d, want %d", s, got, want)
		}
	}
}

// ---------- AssignSpecificityWins ----------

func TestAssign_HostAnchoredCarvesOut(t *testing.T) {
	catalog := []ConcreteBinding{
		{"anthropic", "claude-opus-4-7", "bedrock"},
		{"anthropic", "claude-opus-4-7", "anthropic"},
		{"anthropic", "claude-opus-4-7", "vertex"},
		{"anthropic", "claude-sonnet-4-6", "bedrock"},
		{"anthropic", "claude-sonnet-4-6", "anthropic"},
	}
	groups := []Group[string]{
		{Owner: "A", Refs: []Ref{MustParse("anthropic@bedrock")}},     // 22
		{Owner: "B", Refs: []Ref{MustParse("anthropic/claude-opus-4-7")}}, // 21
	}
	assigns, carve := AssignSpecificityWins(groups, catalog)

	wantA := []ConcreteBinding{
		{"anthropic", "claude-opus-4-7", "bedrock"},
		{"anthropic", "claude-sonnet-4-6", "bedrock"},
	}
	wantB := []ConcreteBinding{
		{"anthropic", "claude-opus-4-7", "anthropic"},
		{"anthropic", "claude-opus-4-7", "vertex"},
	}
	if !reflect.DeepEqual(assigns["A"], wantA) {
		t.Errorf("A got %v, want %v", assigns["A"], wantA)
	}
	if !reflect.DeepEqual(assigns["B"], wantB) {
		t.Errorf("B got %v, want %v", assigns["B"], wantB)
	}
	// One carveout: the opus@bedrock binding (A wins over B).
	if len(carve) != 1 || carve[0].Winner != "A" || !reflect.DeepEqual(carve[0].Losers, []string{"B"}) {
		t.Errorf("carveouts = %v", carve)
	}
	if carve[0].Binding.Host != "bedrock" || carve[0].Binding.Model != "claude-opus-4-7" {
		t.Errorf("wrong carveout binding: %v", carve[0].Binding)
	}
}

func TestAssign_DeclarationOrderBreaksTies(t *testing.T) {
	catalog := []ConcreteBinding{{"anthropic", "claude-opus-4-7", "bedrock"}}
	// Both refs are identical → same score; A declared first wins.
	groups := []Group[string]{
		{Owner: "A", Refs: []Ref{MustParse("anthropic/claude-opus-4-7@bedrock")}},
		{Owner: "B", Refs: []Ref{MustParse("anthropic/claude-opus-4-7@bedrock")}},
	}
	assigns, carve := AssignSpecificityWins(groups, catalog)
	if len(assigns["A"]) != 1 || len(assigns["B"]) != 0 {
		t.Errorf("tie should go to A: A=%d B=%d", len(assigns["A"]), len(assigns["B"]))
	}
	if len(carve) != 1 || carve[0].Winner != "A" {
		t.Errorf("expected carveout with A winner: %v", carve)
	}
}

func TestAssign_UncoveredBindingsStayUnassigned(t *testing.T) {
	catalog := []ConcreteBinding{
		{"anthropic", "claude-opus-4-7", "bedrock"},
		{"openai", "gpt-5", "openai"},
	}
	groups := []Group[string]{
		{Owner: "A", Refs: []Ref{MustParse("anthropic")}},
	}
	assigns, carve := AssignSpecificityWins(groups, catalog)
	if len(assigns["A"]) != 1 {
		t.Errorf("A should own 1 binding, got %d", len(assigns["A"]))
	}
	if len(carve) != 0 {
		t.Errorf("no carveouts expected, got %v", carve)
	}
}

func TestAssign_GroupScoreIsMaxOverRefs(t *testing.T) {
	catalog := []ConcreteBinding{{"anthropic", "claude-opus-4-7", "bedrock"}}
	// A has a broad ref + a narrow one; narrow ref score (33) wins over B's medium ref (22).
	groups := []Group[string]{
		{Owner: "A", Refs: []Ref{MustParse("anthropic"), MustParse("anthropic/claude-opus-4-7@bedrock")}},
		{Owner: "B", Refs: []Ref{MustParse("anthropic@bedrock")}},
	}
	assigns, carve := AssignSpecificityWins(groups, catalog)
	if len(assigns["A"]) != 1 || len(assigns["B"]) != 0 {
		t.Errorf("A should win via narrow ref: A=%d B=%d", len(assigns["A"]), len(assigns["B"]))
	}
	if len(carve) != 1 || carve[0].Winner != "A" || carve[0].Losers[0] != "B" {
		t.Errorf("expected carveout A vs B: %v", carve)
	}
}

func TestAssignFirstWins(t *testing.T) {
	catalog := []ConcreteBinding{{"anthropic", "claude-opus-4-7", "bedrock"}}
	groups := []Group[string]{
		{Owner: "B", Refs: []Ref{MustParse("anthropic/claude-opus-4-7")}}, // narrower
		{Owner: "A", Refs: []Ref{MustParse("anthropic@bedrock")}},         // broader but second
	}
	assigns, conflicts := AssignFirstWins(groups, catalog)
	if len(assigns["B"]) != 1 || len(assigns["A"]) != 0 {
		t.Errorf("first declared (B) should win: A=%d B=%d", len(assigns["A"]), len(assigns["B"]))
	}
	if len(conflicts) != 1 || conflicts[0].Winner != "B" || conflicts[0].Losers[0] != "A" {
		t.Errorf("expected conflict B wins, A loses: %v", conflicts)
	}
}

func TestResolve(t *testing.T) {
	catalog := []ConcreteBinding{
		{"anthropic", "claude-opus-4-7", "anthropic"},
		{"anthropic", "claude-opus-4-7", "bedrock"},
		{"openai", "gpt-5", "openai"},
	}
	refs := []Ref{MustParse("anthropic"), MustParse("openai/gpt-5")}
	out := Resolve(refs, catalog)
	if len(out["anthropic"]) != 2 {
		t.Errorf("anthropic should match 2 bindings, got %d", len(out["anthropic"]))
	}
	if len(out["openai/gpt-5"]) != 1 {
		t.Errorf("openai/gpt-5 should match 1 binding, got %d", len(out["openai/gpt-5"]))
	}
}

// silence unused-import warning for strings on platforms without it.
var _ = strings.Contains
