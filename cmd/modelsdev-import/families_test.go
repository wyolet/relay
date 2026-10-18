package main

import (
	"os"
	"path/filepath"
	"testing"
)

const testFamiliesYAML = `families:
  - name: claude
  - name: command
  - name: gemini
  - name: glm
  - name: gpt
    aliases: [chatgpt, o, o-mini]
  - name: gpt-oss
  - name: mistral
    aliases: [codestral]
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
		if got := f.resolve(tc.in); got != tc.want {
			t.Errorf("resolve(%q) = %q, want %q", tc.in, got, tc.want)
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
