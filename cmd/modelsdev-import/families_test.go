package main

import (
	"os"
	"path/filepath"
	"testing"
)

const testFamiliesYAML = `families:
  - name: claude
    lines:
      - name: opus
      - name: sonnet
  - name: command
  - name: gemini
    lines:
      - name: flash
      - name: flash-lite
      - name: pro
  - name: glm
  - name: gpt
    aliases: [chatgpt, o, o-mini]
    lines:
      - name: o
        aliases: [o-mini, o-pro]
      - name: codex
  - name: gpt-oss
  - name: mistral
    aliases: [codestral]
    lines:
      - name: codestral
  - name: olmo
  - name: qwen
`

func testFamilies(t *testing.T) *familyList {
	t.Helper()
	path := filepath.Join(t.TempDir(), "families.yaml")
	if err := os.WriteFile(path, []byte(testFamiliesYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := loadFamilies(path)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestResolveFamily(t *testing.T) {
	f := testFamilies(t)
	for _, tc := range []struct{ in, want string }{
		{"claude", "claude"},                 // exact name
		{"o-mini", "gpt"},                    // exact alias
		{"codestral", "mistral"},             // exact alias
		{"claude-opus", "claude"},            // name + "-"
		{"gemini-flash-lite", "gemini"},      // name + "-"
		{"command-r", "command"},             // name + "-", no alias needed
		{"qwen3.6", "qwen"},                  // name + digit
		{"glm.5", "glm"},                     // name + "."
		{"gpt-oss-safeguard", "gpt-oss"},     // longest name wins
		{"o3-mini", "gpt"},                   // alias + digit
		{"codestral-embed", "mistral"},       // alias + "-"
		{"olmo", "olmo"},                     // exact beats alias prefix "o"
		{"omni", ""},                         // "o" + letter is not a lineage boundary
		{"qwenish", ""},                      // name + letter is not a lineage boundary
		{"llama", ""},                        // no match
		{"", ""},                             // empty
		{"  Claude-Sonnet ", "claude"},       // case and whitespace
		{"gpt", "gpt"},                       // exact name shadows longer names
		{"chatgpt-4o-latest-x", "gpt"},       // alias + "-"
		{"gpt-oss-120b-instruct", "gpt-oss"}, // longest name with nested dashes
	} {
		if got, _ := f.resolve(tc.in); got != tc.want {
			t.Errorf("resolve(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolveProductLine(t *testing.T) {
	f := testFamilies(t)
	for _, tc := range []struct {
		in, family, candidate, line string
		unmapped                    bool
	}{
		{"claude", "claude", "", "", false},              // exact family: no candidate
		{"claude-opus", "claude", "opus", "opus", false}, // name + "-" remainder
		{"gemini-flash-lite", "gemini", "flash-lite", "flash-lite", false},
		{"gemini-flash", "gemini", "flash", "flash", false},
		{"gpt-codex-mini", "gpt", "codex-mini", "codex", false},         // trailing segment dropped
		{"codestral", "mistral", "codestral", "codestral", false},       // exact family alias
		{"codestral-embed", "mistral", "codestral", "codestral", false}, // alias prefix: the alias is the candidate
		{"o-mini", "gpt", "o-mini", "o", false},                         // line alias
		{"o3-mini", "gpt", "o", "o", false},                             // alias + digit
		{"qwen3.6", "qwen", "3.6", "", false},                           // version, not a line
		{"glm.5", "glm", "5", "", false},                                // version after "."
		{"claude-haiku", "claude", "haiku", "", true},                   // no such line
		{"gemini-ultra-x", "gemini", "ultra-x", "", true},               // no prefix matches either
		{"chatgpt-4o", "gpt", "chatgpt", "", true},                      // alias that is not a line
		{"command-r", "command", "r", "", true},                         // family without lines
		{"mystery", "", "", "", false},                                  // unmapped family, not a line miss
	} {
		family, candidate := f.resolve(tc.in)
		line := f.line(family, candidate)
		unmapped := line == "" && candidate != "" && !isVersion(candidate)
		if family != tc.family || candidate != tc.candidate || line != tc.line || unmapped != tc.unmapped {
			t.Errorf("%q: family=%q candidate=%q line=%q unmapped=%v, want %q %q %q %v",
				tc.in, family, candidate, line, unmapped, tc.family, tc.candidate, tc.line, tc.unmapped)
		}
	}
}

func TestLoadFamiliesMissing(t *testing.T) {
	if _, err := loadFamilies(filepath.Join(t.TempDir(), "families.yaml")); err == nil {
		t.Fatal("missing families.yaml: want error")
	}
}

// TestModelFamilyMapped checks spec.family carries the resolved family, the
// family label is gone, and unmatched models are reported.
func TestModelFamilyMapped(t *testing.T) {
	r, _ := Translate([]MDProvider{prov("p", "@ai-sdk/openai-compatible", map[string]MDModel{
		"mapped":   {ID: "mapped", Family: "gemini-flash"},
		"unmapped": {ID: "unmapped", Family: "mystery"},
	})}, Opts{Families: testFamilies(t)})

	got := map[string]string{}
	for _, m := range r.Models {
		got[m.Metadata.Name] = m.Spec.Family
		if _, ok := m.Metadata.Labels["family"]; ok {
			t.Errorf("model %q still carries a family label", m.Metadata.Name)
		}
	}
	if got["mapped"] != "gemini" || got["unmapped"] != "" {
		t.Errorf("spec.family = %v, want mapped=gemini unmapped=\"\"", got)
	}
	want := UnmappedFamily{Provider: "p", Model: "unmapped", Value: "mystery"}
	if len(r.UnmappedFamilies) != 1 || r.UnmappedFamilies[0] != want {
		t.Errorf("UnmappedFamilies = %+v, want [%+v]", r.UnmappedFamilies, want)
	}
}

// TestModelProductLineMapped checks spec.productLine carries the resolved line
// and only a named-but-unlisted line is reported.
func TestModelProductLineMapped(t *testing.T) {
	r, _ := Translate([]MDProvider{prov("p", "@ai-sdk/openai-compatible", map[string]MDModel{
		"lined":   {ID: "lined", Family: "gemini-flash-lite"},
		"bare":    {ID: "bare", Family: "claude"},
		"version": {ID: "version", Family: "qwen3.6"},
		"unknown": {ID: "unknown", Family: "claude-haiku"},
	})}, Opts{Families: testFamilies(t)})

	got := map[string]string{}
	for _, m := range r.Models {
		got[m.Metadata.Name] = m.Spec.ProductLine
	}
	if got["lined"] != "flash-lite" || got["bare"] != "" || got["version"] != "" || got["unknown"] != "" {
		t.Errorf("spec.productLine = %v, want lined=flash-lite, others empty", got)
	}
	want := UnmappedLine{Provider: "p", Model: "unknown", Value: "claude-haiku"}
	if len(r.UnmappedLines) != 1 || r.UnmappedLines[0] != want {
		t.Errorf("UnmappedLines = %+v, want [%+v]", r.UnmappedLines, want)
	}
	if len(r.UnmappedFamilies) != 0 {
		t.Errorf("UnmappedFamilies = %+v, want none", r.UnmappedFamilies)
	}
}
