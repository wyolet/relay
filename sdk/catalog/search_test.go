package catalog

import (
	"reflect"
	"testing"
)

func TestSearch(t *testing.T) {
	ic, err := Index(&Catalog{
		Hosts: []Host{
			{Name: "openrouter", Models: []Binding{
				{Name: "anthropic/claude-opus-5-5", MetadataName: "claude-opus-5-5", Providers: []string{"anthropic"}},
				{Name: "z-ai/glm-5-3", MetadataName: "glm-5-3", Providers: []string{"zhipu"}},
			}},
			{Name: "openai", Models: []Binding{
				{Name: "gpt-5.5-2026-04-23", MetadataName: "gpt-5-5-2026-04-23", Providers: []string{"openai"}},
			}},
		},
		Models: []ModelInfo{
			{MetadataName: "claude-opus-5-5", Provider: "anthropic", DisplayName: "Claude Opus 5.5"},
			{MetadataName: "gpt-5-5-2026-04-23", Provider: "openai", DisplayName: "GPT-5.5"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	opus := SearchHit{Model: "claude-opus-5-5", Host: "openrouter", DisplayName: "Claude Opus 5.5"}
	glm := SearchHit{Model: "glm-5-3", Host: "openrouter"}
	gpt := SearchHit{Model: "gpt-5-5-2026-04-23", Host: "openai", DisplayName: "GPT-5.5"}
	cases := []struct {
		text string
		want []SearchHit
	}{
		{"OPUS", []SearchHit{opus}},
		{"gpt-5.5", []SearchHit{gpt}},
		{"z-ai/", []SearchHit{glm}},
		{"zhipu", []SearchHit{glm}},
		{"anthropic", []SearchHit{opus}},
		{"5", []SearchHit{opus, glm, gpt}},
		{"", []SearchHit{opus, glm, gpt}},
		{"no-such", nil},
	}
	for _, tc := range cases {
		if got := ic.Search(tc.text); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Search(%q) = %+v, want %+v", tc.text, got, tc.want)
		}
	}
}
