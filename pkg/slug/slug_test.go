package slug

import (
	"strings"
	"testing"
)

func TestFrom(t *testing.T) {
	cases := map[string]string{
		"OpenAI Prod":           "openai-prod",
		"  hello   world  ":     "hello-world",
		"Foo!!!Bar":             "foo-bar",
		"---weird---":           "weird",
		"":                      "",
		"已经-Mixed":              "mixed",
		strings.Repeat("a", 80): strings.Repeat("a", 63),
	}
	for in, want := range cases {
		if got := From(in); got != want {
			t.Errorf("From(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFromPrefix(t *testing.T) {
	cases := map[string]string{
		"claude-fable-5[": "claude-fable-5-", // boundary dash kept
		"claude-fable-5":  "claude-fable-5",
		"ft:":             "ft-",
		"---x":            "x",
		"":                "",
		"[[":              "",
	}
	for in, want := range cases {
		if got := FromPrefix(in); got != want {
			t.Errorf("FromPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFromSuffix(t *testing.T) {
	cases := map[string]string{
		"]":      "",
		":acme":  "-acme", // boundary dash kept
		"acme":   "acme",
		"-mini]": "-mini",
		"":       "",
	}
	for in, want := range cases {
		if got := FromSuffix(in); got != want {
			t.Errorf("FromSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}

// Pins the equivalence model-alias resolution depends on: the CC-style
// bracket variant normalizes to the same key an operator would guess.
func TestFromBracketVariant(t *testing.T) {
	if got := From("claude-fable-5[1m]"); got != "claude-fable-5-1m" {
		t.Fatalf(`From("claude-fable-5[1m]") = %q, want "claude-fable-5-1m"`, got)
	}
}

func TestUnique(t *testing.T) {
	taken := map[string]bool{"openai-prod": true, "openai-prod-2": true}
	exists := func(s string) bool { return taken[s] }
	if got := Unique("openai-prod", exists); got != "openai-prod-3" {
		t.Errorf("got %q, want openai-prod-3", got)
	}
	if got := Unique("anthropic-prod", exists); got != "anthropic-prod" {
		t.Errorf("got %q, want anthropic-prod", got)
	}
}

// A suffixed slug still fits MaxLen: the base gives way, never the suffix, and no dash is left doubled.
func TestWithSuffix(t *testing.T) {
	for _, tc := range []struct {
		base string
		n    int
		want string
	}{
		{"openai-prod", 2, "openai-prod-2"},
		{strings.Repeat("a", 61), 2, strings.Repeat("a", 61) + "-2"},
		{strings.Repeat("a", 63), 2, strings.Repeat("a", 61) + "-2"},
		{strings.Repeat("a", 63), 100, strings.Repeat("a", 59) + "-100"},
		{strings.Repeat("a", 60) + "-bc", 2, strings.Repeat("a", 60) + "-2"},
	} {
		got := WithSuffix(tc.base, tc.n)
		if got != tc.want || !Valid(got) {
			t.Errorf("WithSuffix(%q, %d) = %q (valid %v), want %q", tc.base, tc.n, got, Valid(got), tc.want)
		}
	}
}

func TestValid(t *testing.T) {
	for s, want := range map[string]bool{
		"openai-prod":           true,
		"a":                     true,
		"0":                     true,
		"a-0":                   true,
		"a--b":                  true,
		strings.Repeat("a", 63): true,
		strings.Repeat("a", 64): false,
		"":                      false,
		"OpenAI":                false,
		"-leading":              false,
		"trailing-":             false,
		"-":                     false,
		"under_score":           false,
		"dot.ted":               false,
		"sp ace":                false,
		"ünï":                   false,
		"a\n":                   false,
	} {
		if got := Valid(s); got != want {
			t.Errorf("Valid(%q) = %v, want %v", s, got, want)
		}
	}
	// Whatever From makes of free text is a valid slug or empty.
	for _, in := range []string{"OpenAI Prod", strings.Repeat("ab-", 40), strings.Repeat("a", 62) + "--b", "---", "已经"} {
		if got := From(in); got != "" && !Valid(got) {
			t.Errorf("From(%q) = %q, which is not a valid slug", in, got)
		}
	}
}
