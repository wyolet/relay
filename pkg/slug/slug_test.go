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

func TestValid(t *testing.T) {
	if !Valid("openai-prod") {
		t.Fatal("openai-prod should be valid")
	}
	if Valid("OpenAI") {
		t.Fatal("OpenAI should be invalid (uppercase)")
	}
	if Valid("-leading") || Valid("trailing-") {
		t.Fatal("dashes at edges invalid")
	}
}
