package modelref

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParse(t *testing.T) {
	longest := strings.Repeat("a", 63)
	for _, tc := range []struct {
		in   string
		want Ref
		kind Kind
	}{
		{"anthropic", Ref{Provider: "anthropic", ModelWildcard: true, HostWildcard: true}, KindProvider},
		{"anthropic/*", Ref{Provider: "anthropic", ModelWildcard: true, HostWildcard: true}, KindProvider},
		{"anthropic@bedrock", Ref{Provider: "anthropic", Host: "bedrock", ModelWildcard: true}, KindProviderOnHost},
		{"anthropic/*@bedrock", Ref{Provider: "anthropic", Host: "bedrock", ModelWildcard: true}, KindProviderOnHost},
		{"anthropic/claude-opus-4-7", Ref{Provider: "anthropic", Model: "claude-opus-4-7", HostWildcard: true}, KindModel},
		{"anthropic/claude-opus-4-7@bedrock", Ref{Provider: "anthropic", Model: "claude-opus-4-7", Host: "bedrock"}, KindBinding},
		{"amazon-bedrock/claude-opus-4-7@amazon-bedrock", Ref{Provider: "amazon-bedrock", Model: "claude-opus-4-7", Host: "amazon-bedrock"}, KindBinding},
		{"@bedrock", Ref{Host: "bedrock", ProviderWildcard: true, ModelWildcard: true}, KindHost},
		// Segments are slug-normalized; the / and @ separators are not.
		{"openai/gpt-5.5", Ref{Provider: "openai", Model: "gpt-5-5", HostWildcard: true}, KindModel},
		{"OpenAI/GPT-4o", Ref{Provider: "openai", Model: "gpt-4o", HostWildcard: true}, KindModel},
		{"openai/ft:gpt-3.5-turbo", Ref{Provider: "openai", Model: "ft-gpt-3-5-turbo", HostWildcard: true}, KindModel},
		{"anthropic/claude-3@Bedrock", Ref{Provider: "anthropic", Model: "claude-3", Host: "bedrock"}, KindBinding},
		{"@Amazon_Bedrock", Ref{Host: "amazon-bedrock", ProviderWildcard: true, ModelWildcard: true}, KindHost},
		{"meta/llama/3.1", Ref{Provider: "meta", Model: "llama-3-1", HostWildcard: true}, KindModel},
		{"a/b", Ref{Provider: "a", Model: "b", HostWildcard: true}, KindModel},
		{longest, Ref{Provider: longest, ModelWildcard: true, HostWildcard: true}, KindProvider},
		// A longer segment is cut to the slug length, as the catalog's own slugs are.
		{longest + "b", Ref{Provider: longest, ModelWildcard: true, HostWildcard: true}, KindProvider},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := Parse(tc.in)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			tc.want.Raw = tc.in
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Ref (-want +got):\n%s", diff)
			}
			if got.Kind() != tc.kind {
				t.Errorf("Kind = %q, want %q", got.Kind(), tc.kind)
			}
			if err := Validate(tc.in); err != nil {
				t.Errorf("Validate: %v", err)
			}
			if diff := cmp.Diff(got, MustParse(tc.in)); diff != "" {
				t.Errorf("MustParse differs from Parse (-Parse +MustParse):\n%s", diff)
			}
		})
	}
}

func TestParseRejections(t *testing.T) {
	const noStar = "refs take no patterns; leave the "
	for _, tc := range []struct{ in, reason string }{
		{"/foo", "leading / — provider is required"},
		{"/", "leading / — provider is required"},
		{"anthropic/", "trailing / requires a model"},
		{"anthropic/@bedrock", "trailing / requires a model"},
		{"anthropic/claude@", "trailing @ requires a host"},
		{"anthropic@", "trailing @ requires a host"},
		{"@", "trailing @ requires a host"},
		// A model of * is the one pattern spelling; a lone * anywhere else names nothing.
		{"*", "provider has no slug-compatible characters"},
		{"*/gpt-4o", "provider has no slug-compatible characters"},
		{"anthropic/claude@*", "host has no slug-compatible characters"},
		{"@*", "host has no slug-compatible characters"},
		{"anthropic/...", "model has no slug-compatible characters"},
		{" ", "provider has no slug-compatible characters"},
		// A * beside other characters would normalize to a literal prefix that grants nothing.
		{"stress-mock/stress-*", `model "stress-*" contains *: ` + noStar + "model out to match every one"},
		{"anthropic/claude-*@bedrock", `model "claude-*" contains *: ` + noStar + "model out to match every one"},
		{"anthropic/claude@aws-*", `host "aws-*" contains *: ` + noStar + "host out to match every one"},
		{"open*", `provider "open*" contains *: ` + noStar + "provider out to match every one"},
		{"@bed*rock", `host "bed*rock" contains *: ` + noStar + "host out to match every one"},
		{"anthropic/**", "model has no slug-compatible characters"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			_, err := Parse(tc.in)
			var se *SyntaxError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v (%T), want *SyntaxError", err, err)
			}
			if diff := cmp.Diff(SyntaxError{Raw: tc.in, Reason: tc.reason}, *se); diff != "" {
				t.Errorf("SyntaxError (-want +got):\n%s", diff)
			}
			if want := `modelref "` + tc.in + `": ` + tc.reason; err.Error() != want {
				t.Errorf("Error() = %q, want %q", err.Error(), want)
			}
			if Validate(tc.in) == nil {
				t.Error("Validate accepted it")
			}
		})
	}
	if _, err := Parse(""); !errors.Is(err, ErrEmpty) {
		t.Errorf(`Parse(""): err = %v, want ErrEmpty`, err)
	}
	defer func() {
		if recover() == nil {
			t.Error("MustParse of a malformed ref did not panic")
		}
	}()
	MustParse("anthropic/")
}

func TestParseRejectsARepeatedSeparator(t *testing.T) {
	for _, in := range []string{"anthropic@bedrock@vertex", "@bedrock@vertex", "anthropic/claude@bedrock@vertex"} {
		got, err := Parse(in)
		var se *SyntaxError
		if !errors.As(err, &se) {
			t.Errorf("Parse(%q) = %+v, %v; want a *SyntaxError", in, got, err)
		}
	}
}

func TestMatches(t *testing.T) {
	bindings := []ConcreteBinding{
		{"anthropic", "claude-opus-4-7", "anthropic"},
		{"anthropic", "claude-opus-4-7", "bedrock"},
		{"anthropic", "claude-haiku-4-5", "bedrock"},
		{"openai", "gpt-4o", "bedrock"},
		{"openai", "gpt-4o", "openai"},
		{"openai", "claude-opus-4-7", "anthropic"},
	}
	// One flag per binding above, in order.
	for ref, want := range map[string][]bool{
		"@bedrock":                          {false, true, true, true, false, false},
		"anthropic":                         {true, true, true, false, false, false},
		"anthropic/*":                       {true, true, true, false, false, false},
		"anthropic@bedrock":                 {false, true, true, false, false, false},
		"anthropic/claude-opus-4-7":         {true, true, false, false, false, false},
		"anthropic/claude-opus-4-7@bedrock": {false, true, false, false, false, false},
		"Anthropic/Claude.Opus.4.7@Bedrock": {false, true, false, false, false, false},
	} {
		r := MustParse(ref)
		got := make([]bool, len(bindings))
		for i, b := range bindings {
			got[i] = r.Matches(b.Provider, b.Model, b.Host)
			if covers := r.Covers(b); covers != got[i] {
				t.Errorf("%s: Covers(%v) = %v, Matches = %v", ref, b, covers, got[i])
			}
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("%s matches (-want +got):\n%s", ref, diff)
		}
	}
}

func TestMatchAny(t *testing.T) {
	for _, tc := range []struct {
		name string
		refs []string
		want bool
	}{
		{"no refs", nil, false},
		{"one matching ref", []string{"openai", "anthropic@bedrock"}, true},
		{"none matching", []string{"openai", "anthropic@vertex", "anthropic/claude-haiku-4-5"}, false},
		{"malformed refs are skipped, not matched", []string{"", "anthropic/", "*", "anthropic/claude-*"}, false},
		{"a malformed ref does not stop a later match", []string{"anthropic/", "@bedrock"}, true},
	} {
		if got := MatchAny(tc.refs, "anthropic", "claude-opus-4-7", "bedrock"); got != tc.want {
			t.Errorf("%s: MatchAny = %v, want %v", tc.name, got, tc.want)
		}
	}
}
