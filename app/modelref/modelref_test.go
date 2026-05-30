package modelref

import (
	"errors"
	"testing"
)

func TestParse_ValidShapes(t *testing.T) {
	cases := []struct {
		in               string
		provider         string
		model            string
		host             string
		providerWildcard bool
		modelWildcard    bool
		hostWildcard     bool
		kind             Kind
	}{
		{"anthropic", "anthropic", "", "", false, true, true, KindProvider},
		{"anthropic@bedrock", "anthropic", "", "bedrock", false, true, false, KindProviderOnHost},
		{"anthropic/claude-opus-4-7", "anthropic", "claude-opus-4-7", "", false, false, true, KindModel},
		{"anthropic/claude-opus-4-7@bedrock", "anthropic", "claude-opus-4-7", "bedrock", false, false, false, KindBinding},
		{"amazon-bedrock/claude-opus-4-7@amazon-bedrock", "amazon-bedrock", "claude-opus-4-7", "amazon-bedrock", false, false, false, KindBinding},
		{"@bedrock", "", "", "bedrock", true, true, false, KindHost},
		{"@amazon-bedrock", "", "", "amazon-bedrock", true, true, false, KindHost},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			r, err := Parse(tc.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if r.Provider != tc.provider {
				t.Errorf("provider: want %q got %q", tc.provider, r.Provider)
			}
			if r.Model != tc.model {
				t.Errorf("model: want %q got %q", tc.model, r.Model)
			}
			if r.Host != tc.host {
				t.Errorf("host: want %q got %q", tc.host, r.Host)
			}
			if r.ProviderWildcard != tc.providerWildcard {
				t.Errorf("providerWildcard: want %v got %v", tc.providerWildcard, r.ProviderWildcard)
			}
			if r.ModelWildcard != tc.modelWildcard {
				t.Errorf("modelWildcard: want %v got %v", tc.modelWildcard, r.ModelWildcard)
			}
			if r.HostWildcard != tc.hostWildcard {
				t.Errorf("hostWildcard: want %v got %v", tc.hostWildcard, r.HostWildcard)
			}
			if r.Kind() != tc.kind {
				t.Errorf("kind: want %s got %s", tc.kind, r.Kind())
			}
			if r.Raw != tc.in {
				t.Errorf("raw: want %q got %q", tc.in, r.Raw)
			}
		})
	}
}

func TestParse_Errors(t *testing.T) {
	cases := []struct {
		in     string
		errIs  error
		reason string // substring of *SyntaxError.Reason; empty means any
	}{
		{"", ErrEmpty, ""},
		{"/foo", nil, "leading /"},
		{"anthropic/", nil, "trailing /"},
		{"anthropic/claude@", nil, "trailing @"},
		{"@", nil, "trailing @"},
		// `*` has no slug-compatible characters → rejected after normalization.
		{"anthropic/*", nil, "slug-compatible"},
		{"anthropic/*@bedrock", nil, "slug-compatible"},
		{"anthropic/claude@*", nil, "slug-compatible"},
		{"@*", nil, "slug-compatible"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			_, err := Parse(tc.in)
			if err == nil {
				t.Fatalf("expected error for %q", tc.in)
			}
			if tc.errIs != nil {
				if !errors.Is(err, tc.errIs) {
					t.Errorf("want errors.Is(%v), got %v", tc.errIs, err)
				}
				return
			}
			var se *SyntaxError
			if !errors.As(err, &se) {
				t.Fatalf("want *SyntaxError, got %T: %v", err, err)
			}
			if tc.reason != "" && !contains(se.Reason, tc.reason) {
				t.Errorf("reason: want substring %q, got %q", tc.reason, se.Reason)
			}
		})
	}
}

func TestParse_NormalizesToSlug(t *testing.T) {
	cases := []struct {
		in       string
		provider string
		model    string
		host     string
	}{
		{"openai/gpt-5.5", "openai", "gpt-5-5", ""},
		{"OpenAI/GPT-4o", "openai", "gpt-4o", ""},
		{"openai/ft:gpt-3.5-turbo", "openai", "ft-gpt-3-5-turbo", ""},
		{"anthropic/claude-3@Bedrock", "anthropic", "claude-3", "bedrock"},
		{"@Amazon-Bedrock", "", "", "amazon-bedrock"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			ref, err := Parse(tc.in)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.in, err)
			}
			if ref.Provider != tc.provider {
				t.Errorf("Provider = %q, want %q", ref.Provider, tc.provider)
			}
			if ref.Model != tc.model {
				t.Errorf("Model = %q, want %q", ref.Model, tc.model)
			}
			if ref.Host != tc.host {
				t.Errorf("Host = %q, want %q", ref.Host, tc.host)
			}
		})
	}
}

func TestMatches(t *testing.T) {
	type triple struct{ p, m, h string }
	cases := []struct {
		ref  string
		want map[triple]bool
	}{
		{"@bedrock", map[triple]bool{
			{"anthropic", "claude-opus-4-7", "bedrock"}:   true,
			{"openai", "gpt-4o", "bedrock"}:               true,
			{"anthropic", "claude-opus-4-7", "anthropic"}: false,
		}},
		{"anthropic", map[triple]bool{
			{"anthropic", "claude-opus-4-7", "anthropic"}: true,
			{"anthropic", "claude-haiku-4-5", "bedrock"}:  true,
			{"openai", "gpt-4o", "openai"}:                false,
		}},
		{"anthropic/claude-opus-4-7", map[triple]bool{
			{"anthropic", "claude-opus-4-7", "anthropic"}: true,
			{"anthropic", "claude-opus-4-7", "bedrock"}:   true,
			{"anthropic", "claude-haiku-4-5", "bedrock"}:  false,
		}},
		{"anthropic/claude-opus-4-7@bedrock", map[triple]bool{
			{"anthropic", "claude-opus-4-7", "bedrock"}:   true,
			{"anthropic", "claude-opus-4-7", "anthropic"}: false,
			{"anthropic", "claude-haiku-4-5", "bedrock"}:  false,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.ref, func(t *testing.T) {
			r := MustParse(tc.ref)
			for tr, want := range tc.want {
				if got := r.Matches(tr.p, tr.m, tr.h); got != want {
					t.Errorf("%s.Matches(%q,%q,%q) = %v, want %v", tc.ref, tr.p, tr.m, tr.h, got, want)
				}
			}
		})
	}
}

func contains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
